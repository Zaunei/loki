package bloomcompactor

import (
	"context"
	"fmt"
	"io"
	"math"
	"path"
	"strings"

	"github.com/pkg/errors"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"

	baseStore "github.com/grafana/loki/pkg/storage"
	v1 "github.com/grafana/loki/pkg/storage/bloom/v1"
	"github.com/grafana/loki/pkg/storage/config"
	"github.com/grafana/loki/pkg/storage/stores/shipper/indexshipper/storage"
	"github.com/grafana/loki/pkg/storage/stores/shipper/indexshipper/tsdb"
	"github.com/grafana/loki/pkg/storage/stores/shipper/indexshipper/tsdb/index"
)

const (
	gzipExtension = ".gz"
)

type TSDBStore interface {
	UsersForPeriod(ctx context.Context, table DayTable) ([]string, error)
	ResolveTSDBs(ctx context.Context, table DayTable, tenant string) ([]tsdb.SingleTenantTSDBIdentifier, error)
	LoadTSDB(
		ctx context.Context,
		table DayTable,
		tenant string,
		id tsdb.Identifier,
		bounds v1.FingerprintBounds,
	) (v1.CloseableIterator[*v1.Series], error)
}

// BloomTSDBStore is a wrapper around the storage.Client interface which
// implements the TSDBStore interface for this pkg.
type BloomTSDBStore struct {
	storage storage.Client
}

func NewBloomTSDBStore(storage storage.Client) *BloomTSDBStore {
	return &BloomTSDBStore{
		storage: storage,
	}
}

func (b *BloomTSDBStore) UsersForPeriod(ctx context.Context, table DayTable) ([]string, error) {
	_, users, err := b.storage.ListFiles(ctx, table.String(), false)
	return users, err
}

func (b *BloomTSDBStore) ResolveTSDBs(ctx context.Context, table DayTable, tenant string) ([]tsdb.SingleTenantTSDBIdentifier, error) {
	indices, err := b.storage.ListUserFiles(ctx, table.String(), tenant, false)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list user files")
	}

	ids := make([]tsdb.SingleTenantTSDBIdentifier, 0, len(indices))
	for _, index := range indices {
		key := index.Name
		if decompress := storage.IsCompressedFile(index.Name); decompress {
			key = strings.TrimSuffix(key, gzipExtension)
		}

		id, ok := tsdb.ParseSingleTenantTSDBPath(path.Base(key))
		if !ok {
			return nil, errors.Errorf("failed to parse single tenant tsdb path: %s", key)
		}

		ids = append(ids, id)

	}
	return ids, nil
}

func (b *BloomTSDBStore) LoadTSDB(
	ctx context.Context,
	table DayTable,
	tenant string,
	id tsdb.Identifier,
	bounds v1.FingerprintBounds,
) (v1.CloseableIterator[*v1.Series], error) {
	data, err := b.storage.GetUserFile(ctx, table.String(), tenant, id.Name())
	if err != nil {
		return nil, errors.Wrap(err, "failed to get file")
	}

	buf, err := io.ReadAll(data)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read file")
	}
	_ = data.Close()

	reader, err := index.NewReader(index.RealByteSlice(buf))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create index reader")
	}

	idx := tsdb.NewTSDBIndex(reader)

	return NewTSDBSeriesIter(ctx, idx, bounds), nil
}

// TSDBStore is an interface for interacting with the TSDB,
// modeled off a relevant subset of the `tsdb.TSDBIndex` struct
type forSeries interface {
	ForSeries(
		ctx context.Context,
		fpFilter index.FingerprintFilter,
		from model.Time,
		through model.Time,
		fn func(labels.Labels, model.Fingerprint, []index.ChunkMeta),
		matchers ...*labels.Matcher,
	) error
	Close() error
}

type TSDBSeriesIter struct {
	f      forSeries
	bounds v1.FingerprintBounds
	ctx    context.Context

	ch          chan *v1.Series
	initialized bool
	next        *v1.Series
	err         error
}

func NewTSDBSeriesIter(ctx context.Context, f forSeries, bounds v1.FingerprintBounds) *TSDBSeriesIter {
	return &TSDBSeriesIter{
		f:      f,
		bounds: bounds,
		ctx:    ctx,
		ch:     make(chan *v1.Series),
	}
}

func (t *TSDBSeriesIter) Next() bool {
	if !t.initialized {
		t.initialized = true
		t.background()
	}

	select {
	case <-t.ctx.Done():
		return false
	case next, ok := <-t.ch:
		t.next = next
		return ok
	}
}

func (t *TSDBSeriesIter) At() *v1.Series {
	return t.next
}

func (t *TSDBSeriesIter) Err() error {
	if t.err != nil {
		return t.err
	}

	return t.ctx.Err()
}

func (t *TSDBSeriesIter) Close() error {
	return t.f.Close()
}

// background iterates over the tsdb file, populating the next
// value via a channel to handle backpressure
func (t *TSDBSeriesIter) background() {
	go func() {
		t.err = t.f.ForSeries(
			t.ctx,
			t.bounds,
			0, math.MaxInt64,
			func(_ labels.Labels, fp model.Fingerprint, chks []index.ChunkMeta) {

				res := &v1.Series{
					Fingerprint: fp,
					Chunks:      make(v1.ChunkRefs, 0, len(chks)),
				}
				for _, chk := range chks {
					res.Chunks = append(res.Chunks, v1.ChunkRef{
						Start:    model.Time(chk.MinTime),
						End:      model.Time(chk.MaxTime),
						Checksum: chk.Checksum,
					})
				}

				select {
				case <-t.ctx.Done():
					return
				case t.ch <- res:
				}
			},
			labels.MustNewMatcher(labels.MatchEqual, "", ""),
		)
		close(t.ch)
	}()
}

type TSDBStores struct {
	schemaCfg config.SchemaConfig
	stores    []TSDBStore
}

func NewTSDBStores(
	schemaCfg config.SchemaConfig,
	storeCfg baseStore.Config,
	clientMetrics baseStore.ClientMetrics,
) (*TSDBStores, error) {
	res := &TSDBStores{
		schemaCfg: schemaCfg,
		stores:    make([]TSDBStore, len(schemaCfg.Configs)),
	}

	for i, cfg := range schemaCfg.Configs {
		if cfg.IndexType == config.TSDBType {

			c, err := baseStore.NewObjectClient(cfg.ObjectType, storeCfg, clientMetrics)
			if err != nil {
				return nil, errors.Wrap(err, "failed to create object client")
			}
			res.stores[i] = NewBloomTSDBStore(storage.NewIndexStorageClient(c, cfg.IndexTables.PathPrefix))
		}
	}

	return res, nil
}

func (s *TSDBStores) storeForPeriod(table DayTable) (TSDBStore, error) {
	for i := len(s.schemaCfg.Configs) - 1; i >= 0; i-- {
		period := s.schemaCfg.Configs[i]

		if !table.Before(DayTable(period.From.Time)) {
			// we have the desired period config

			if s.stores[i] != nil {
				// valid: it's of tsdb type
				return s.stores[i], nil
			}

			// invalid
			return nil, errors.Errorf(
				"store for period is not of TSDB type (%s) while looking up store for (%v)",
				period.IndexType,
				table.ModelTime().Time(),
			)
		}

	}

	return nil, fmt.Errorf(
		"There is no store matching no matching period found for table (%v) -- too early",
		table.ModelTime().Time(),
	)
}

func (s *TSDBStores) UsersForPeriod(ctx context.Context, table DayTable) ([]string, error) {
	store, err := s.storeForPeriod(table)
	if err != nil {
		return nil, err
	}

	return store.UsersForPeriod(ctx, table)
}

func (s *TSDBStores) ResolveTSDBs(ctx context.Context, table DayTable, tenant string) ([]tsdb.SingleTenantTSDBIdentifier, error) {
	store, err := s.storeForPeriod(table)
	if err != nil {
		return nil, err
	}

	return store.ResolveTSDBs(ctx, table, tenant)
}

func (s *TSDBStores) LoadTSDB(
	ctx context.Context,
	table DayTable,
	tenant string,
	id tsdb.Identifier,
	bounds v1.FingerprintBounds,
) (v1.CloseableIterator[*v1.Series], error) {
	store, err := s.storeForPeriod(table)
	if err != nil {
		return nil, err
	}

	return store.LoadTSDB(ctx, table, tenant, id, bounds)
}
