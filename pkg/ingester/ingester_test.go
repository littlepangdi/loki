package ingester

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/grafana/dskit/flagext"
	"github.com/grafana/dskit/services"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/stretchr/testify/require"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/user"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/grafana/loki/pkg/chunkenc"
	"github.com/grafana/loki/pkg/ingester/client"
	"github.com/grafana/loki/pkg/ingester/index"
	"github.com/grafana/loki/pkg/iter"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/logql"
	"github.com/grafana/loki/pkg/runtime"
	"github.com/grafana/loki/pkg/storage"
	"github.com/grafana/loki/pkg/storage/chunk"
	"github.com/grafana/loki/pkg/tenant"
	"github.com/grafana/loki/pkg/validation"
)

func TestIngester(t *testing.T) {
	ingesterConfig := defaultIngesterTestConfig(t)
	limits, err := validation.NewOverrides(defaultLimitsTestConfig(), nil)
	require.NoError(t, err)

	store := &mockStore{
		chunks: map[string][]chunk.Chunk{},
	}

	i, err := New(ingesterConfig, client.Config{}, store, limits, runtime.DefaultTenantConfigs(), nil)
	require.NoError(t, err)
	defer services.StopAndAwaitTerminated(context.Background(), i) //nolint:errcheck

	req := logproto.PushRequest{
		Streams: []logproto.Stream{
			{
				Labels: `{foo="bar",bar="baz1"}`,
			},
			{
				Labels: `{foo="bar",bar="baz2"}`,
			},
		},
	}
	for i := 0; i < 10; i++ {
		req.Streams[0].Entries = append(req.Streams[0].Entries, logproto.Entry{
			Timestamp: time.Unix(0, 0),
			Line:      fmt.Sprintf("line %d", i),
		})
		req.Streams[1].Entries = append(req.Streams[1].Entries, logproto.Entry{
			Timestamp: time.Unix(0, 0),
			Line:      fmt.Sprintf("line %d", i),
		})
	}

	ctx := user.InjectOrgID(context.Background(), "test")
	_, err = i.Push(ctx, &req)
	require.NoError(t, err)

	result := mockQuerierServer{
		ctx: ctx,
	}
	err = i.Query(&logproto.QueryRequest{
		Selector: `{foo="bar"}`,
		Limit:    100,
		Start:    time.Unix(0, 0),
		End:      time.Unix(1, 0),
	}, &result)
	require.NoError(t, err)
	require.Len(t, result.resps, 1)
	require.Len(t, result.resps[0].Streams, 2)

	result = mockQuerierServer{
		ctx: ctx,
	}
	err = i.Query(&logproto.QueryRequest{
		Selector: `{foo="bar",bar="baz1"}`,
		Limit:    100,
		Start:    time.Unix(0, 0),
		End:      time.Unix(1, 0),
	}, &result)
	require.NoError(t, err)
	require.Len(t, result.resps, 1)
	require.Len(t, result.resps[0].Streams, 1)
	require.Equal(t, `{bar="baz1", foo="bar"}`, result.resps[0].Streams[0].Labels)

	result = mockQuerierServer{
		ctx: ctx,
	}
	err = i.Query(&logproto.QueryRequest{
		Selector: `{foo="bar",bar="baz2"}`,
		Limit:    100,
		Start:    time.Unix(0, 0),
		End:      time.Unix(1, 0),
	}, &result)
	require.NoError(t, err)
	require.Len(t, result.resps, 1)
	require.Len(t, result.resps[0].Streams, 1)
	require.Equal(t, `{bar="baz2", foo="bar"}`, result.resps[0].Streams[0].Labels)

	// Series

	// empty matcher return all series
	resp, err := i.Series(ctx, &logproto.SeriesRequest{
		Start: time.Unix(0, 0),
		End:   time.Unix(1, 0),
	})
	require.Nil(t, err)
	require.ElementsMatch(t, []logproto.SeriesIdentifier{
		{
			Labels: map[string]string{
				"foo": "bar",
				"bar": "baz1",
			},
		},
		{
			Labels: map[string]string{
				"foo": "bar",
				"bar": "baz2",
			},
		},
	}, resp.GetSeries())

	// wrong matchers fmt
	_, err = i.Series(ctx, &logproto.SeriesRequest{
		Start:  time.Unix(0, 0),
		End:    time.Unix(1, 0),
		Groups: []string{`{a="b`},
	})
	require.Error(t, err)

	// no selectors
	_, err = i.Series(ctx, &logproto.SeriesRequest{
		Start:  time.Unix(0, 0),
		End:    time.Unix(1, 0),
		Groups: []string{`{foo="bar"}`, `{}`},
	})
	require.Error(t, err)

	// foo=bar
	resp, err = i.Series(ctx, &logproto.SeriesRequest{
		Start:  time.Unix(0, 0),
		End:    time.Unix(1, 0),
		Groups: []string{`{foo="bar"}`},
	})
	require.Nil(t, err)
	require.ElementsMatch(t, []logproto.SeriesIdentifier{
		{
			Labels: map[string]string{
				"foo": "bar",
				"bar": "baz1",
			},
		},
		{
			Labels: map[string]string{
				"foo": "bar",
				"bar": "baz2",
			},
		},
	}, resp.GetSeries())

	// foo=bar, bar=~"baz[2-9]"
	resp, err = i.Series(ctx, &logproto.SeriesRequest{
		Start:  time.Unix(0, 0),
		End:    time.Unix(1, 0),
		Groups: []string{`{foo="bar", bar=~"baz[2-9]"}`},
	})
	require.Nil(t, err)
	require.ElementsMatch(t, []logproto.SeriesIdentifier{
		{
			Labels: map[string]string{
				"foo": "bar",
				"bar": "baz2",
			},
		},
	}, resp.GetSeries())

	// foo=bar, bar=~"baz[2-9]" in different groups should OR the results
	resp, err = i.Series(ctx, &logproto.SeriesRequest{
		Start:  time.Unix(0, 0),
		End:    time.Unix(1, 0),
		Groups: []string{`{foo="bar"}`, `{bar=~"baz[2-9]"}`},
	})
	require.Nil(t, err)
	require.ElementsMatch(t, []logproto.SeriesIdentifier{
		{
			Labels: map[string]string{
				"foo": "bar",
				"bar": "baz1",
			},
		},
		{
			Labels: map[string]string{
				"foo": "bar",
				"bar": "baz2",
			},
		},
	}, resp.GetSeries())
}

func TestIngesterStreamLimitExceeded(t *testing.T) {
	ingesterConfig := defaultIngesterTestConfig(t)
	defaultLimits := defaultLimitsTestConfig()
	defaultLimits.MaxLocalStreamsPerUser = 1
	overrides, err := validation.NewOverrides(defaultLimits, nil)

	require.NoError(t, err)

	store := &mockStore{
		chunks: map[string][]chunk.Chunk{},
	}

	i, err := New(ingesterConfig, client.Config{}, store, overrides, runtime.DefaultTenantConfigs(), nil)
	require.NoError(t, err)
	defer services.StopAndAwaitTerminated(context.Background(), i) //nolint:errcheck

	req := logproto.PushRequest{
		Streams: []logproto.Stream{
			{
				Labels: `{foo="bar",bar="baz1"}`,
			},
		},
	}
	for i := 0; i < 10; i++ {
		req.Streams[0].Entries = append(req.Streams[0].Entries, logproto.Entry{
			Timestamp: time.Unix(0, 0),
			Line:      fmt.Sprintf("line %d", i),
		})
	}

	ctx := user.InjectOrgID(context.Background(), "test")
	_, err = i.Push(ctx, &req)
	require.NoError(t, err)

	req.Streams[0].Labels = `{foo="bar",bar="baz2"}`

	_, err = i.Push(ctx, &req)
	if resp, ok := httpgrpc.HTTPResponseFromError(err); !ok || resp.Code != http.StatusTooManyRequests {
		t.Fatalf("expected error about exceeding metrics per user, got %v", err)
	}
}

type mockStore struct {
	mtx    sync.Mutex
	chunks map[string][]chunk.Chunk
}

func (s *mockStore) Put(ctx context.Context, chunks []chunk.Chunk) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	userid, err := tenant.TenantID(ctx)
	if err != nil {
		return err
	}

	s.chunks[userid] = append(s.chunks[userid], chunks...)
	return nil
}

func (s *mockStore) SelectLogs(ctx context.Context, req logql.SelectLogParams) (iter.EntryIterator, error) {
	return nil, nil
}

func (s *mockStore) SelectSamples(ctx context.Context, req logql.SelectSampleParams) (iter.SampleIterator, error) {
	return nil, nil
}

func (s *mockStore) GetSeries(ctx context.Context, req logql.SelectLogParams) ([]logproto.SeriesIdentifier, error) {
	return nil, nil
}

func (s *mockStore) GetSchemaConfigs() []chunk.PeriodConfig {
	return nil
}

func (s *mockStore) SetChunkFilterer(_ storage.RequestChunkFilterer) {
}

// chunk.Store methods
func (s *mockStore) PutOne(ctx context.Context, from, through model.Time, chunk chunk.Chunk) error {
	return nil
}

func (s *mockStore) Get(ctx context.Context, userID string, from, through model.Time, matchers ...*labels.Matcher) ([]chunk.Chunk, error) {
	return nil, nil
}

func (s *mockStore) GetChunkRefs(ctx context.Context, userID string, from, through model.Time, matchers ...*labels.Matcher) ([][]chunk.Chunk, []*chunk.Fetcher, error) {
	return nil, nil, nil
}

func (s *mockStore) LabelValuesForMetricName(ctx context.Context, userID string, from, through model.Time, metricName string, labelName string, matchers ...*labels.Matcher) ([]string, error) {
	return []string{"val1", "val2"}, nil
}

func (s *mockStore) LabelNamesForMetricName(ctx context.Context, userID string, from, through model.Time, metricName string) ([]string, error) {
	return nil, nil
}

func (s *mockStore) GetChunkFetcher(tm model.Time) *chunk.Fetcher {
	return nil
}

func (s *mockStore) DeleteChunk(ctx context.Context, from, through model.Time, userID, chunkID string, metric labels.Labels, partiallyDeletedInterval *model.Interval) error {
	return nil
}

func (s *mockStore) DeleteSeriesIDs(ctx context.Context, from, through model.Time, userID string, metric labels.Labels) error {
	return nil
}

func (s *mockStore) Stop() {}

type mockQuerierServer struct {
	ctx   context.Context
	resps []*logproto.QueryResponse
	grpc.ServerStream
}

func (*mockQuerierServer) SetTrailer(metadata.MD) {}

func (m *mockQuerierServer) Send(resp *logproto.QueryResponse) error {
	m.resps = append(m.resps, resp)
	return nil
}

func (m *mockQuerierServer) Context() context.Context {
	return m.ctx
}

func defaultLimitsTestConfig() validation.Limits {
	limits := validation.Limits{}
	flagext.DefaultValues(&limits)
	return limits
}

func TestIngester_buildStoreRequest(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name                       string
		queryStore                 bool
		maxLookBackPeriod          time.Duration
		start, end                 time.Time
		expectedStart, expectedEnd time.Time
		shouldQuery                bool
	}{
		{
			name:        "do not query store",
			queryStore:  false,
			start:       now.Add(-time.Minute),
			end:         now,
			shouldQuery: false,
		},
		{
			name:              "query store with max look back covering whole request duration",
			queryStore:        true,
			maxLookBackPeriod: time.Hour,
			start:             now.Add(-10 * time.Minute),
			end:               now,
			expectedStart:     now.Add(-10 * time.Minute),
			expectedEnd:       now,
			shouldQuery:       true,
		},
		{
			name:              "query store with max look back covering partial request duration",
			queryStore:        true,
			maxLookBackPeriod: time.Hour,
			start:             now.Add(-2 * time.Hour),
			end:               now,
			expectedStart:     now.Add(-time.Hour),
			expectedEnd:       now,
			shouldQuery:       true,
		},
		{
			name:              "query store with max look back not covering request duration at all",
			queryStore:        true,
			maxLookBackPeriod: time.Hour,
			start:             now.Add(-4 * time.Hour),
			end:               now.Add(-2 * time.Hour),
			shouldQuery:       false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ingesterConfig := defaultIngesterTestConfig(t)
			ingesterConfig.QueryStore = tc.queryStore
			ingesterConfig.QueryStoreMaxLookBackPeriod = tc.maxLookBackPeriod

			start, end, ok := buildStoreRequest(ingesterConfig, tc.start, tc.end, now)

			if !tc.shouldQuery {
				require.False(t, ok)
				return
			}
			require.Equal(t, tc.expectedEnd, end, "end")
			require.Equal(t, tc.expectedStart, start, "start")
		})
	}
}

func TestIngester_boltdbShipperMaxLookBack(t *testing.T) {
	now := model.Now()

	for _, tc := range []struct {
		name                string
		periodicConfigs     []chunk.PeriodConfig
		expectedMaxLookBack time.Duration
	}{
		{
			name: "not using boltdb-shipper",
			periodicConfigs: []chunk.PeriodConfig{
				{
					From:      chunk.DayTime{Time: now.Add(-24 * time.Hour)},
					IndexType: "bigtable",
				},
			},
		},
		{
			name: "just one periodic config with boltdb-shipper",
			periodicConfigs: []chunk.PeriodConfig{
				{
					From:      chunk.DayTime{Time: now.Add(-24 * time.Hour)},
					IndexType: "boltdb-shipper",
				},
			},
			expectedMaxLookBack: time.Since(now.Add(-24 * time.Hour).Time()),
		},
		{
			name: "active config boltdb-shipper, previous config non boltdb-shipper",
			periodicConfigs: []chunk.PeriodConfig{
				{
					From:      chunk.DayTime{Time: now.Add(-48 * time.Hour)},
					IndexType: "bigtable",
				},
				{
					From:      chunk.DayTime{Time: now.Add(-24 * time.Hour)},
					IndexType: "boltdb-shipper",
				},
			},
			expectedMaxLookBack: time.Since(now.Add(-24 * time.Hour).Time()),
		},
		{
			name: "current and previous config both using boltdb-shipper",
			periodicConfigs: []chunk.PeriodConfig{
				{
					From:      chunk.DayTime{Time: now.Add(-48 * time.Hour)},
					IndexType: "boltdb-shipper",
				},
				{
					From:      chunk.DayTime{Time: now.Add(-24 * time.Hour)},
					IndexType: "boltdb-shipper",
				},
			},
			expectedMaxLookBack: time.Since(now.Add(-48 * time.Hour).Time()),
		},
		{
			name: "active config non boltdb-shipper, previous config boltdb-shipper",
			periodicConfigs: []chunk.PeriodConfig{
				{
					From:      chunk.DayTime{Time: now.Add(-48 * time.Hour)},
					IndexType: "boltdb-shipper",
				},
				{
					From:      chunk.DayTime{Time: now.Add(-24 * time.Hour)},
					IndexType: "bigtable",
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ingester := Ingester{periodicConfigs: tc.periodicConfigs}
			mlb := ingester.boltdbShipperMaxLookBack()
			require.InDelta(t, tc.expectedMaxLookBack, mlb, float64(time.Second))
		})
	}
}

func TestValidate(t *testing.T) {
	for i, tc := range []struct {
		in       Config
		err      bool
		expected Config
	}{
		{
			in: Config{
				MaxChunkAge:   time.Minute,
				ChunkEncoding: chunkenc.EncGZIP.String(),
				IndexShards:   index.DefaultIndexShards,
			},
			expected: Config{
				MaxChunkAge:    time.Minute,
				ChunkEncoding:  chunkenc.EncGZIP.String(),
				parsedEncoding: chunkenc.EncGZIP,
				IndexShards:    index.DefaultIndexShards,
			},
		},
		{
			in: Config{
				ChunkEncoding: chunkenc.EncSnappy.String(),
				IndexShards:   index.DefaultIndexShards,
			},
			expected: Config{
				ChunkEncoding:  chunkenc.EncSnappy.String(),
				parsedEncoding: chunkenc.EncSnappy,
				IndexShards:    index.DefaultIndexShards,
			},
		},
		{
			in: Config{
				IndexShards:   index.DefaultIndexShards,
				ChunkEncoding: "bad-enc",
			},
			err: true,
		},
		{
			in: Config{
				MaxChunkAge:   time.Minute,
				ChunkEncoding: chunkenc.EncGZIP.String(),
			},
			err: true,
		},
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			err := tc.in.Validate()
			if tc.err {
				require.NotNil(t, err)
				return
			}
			require.Nil(t, err)
			require.Equal(t, tc.expected, tc.in)
		})
	}
}

func Test_InMemoryLabels(t *testing.T) {
	ingesterConfig := defaultIngesterTestConfig(t)
	limits, err := validation.NewOverrides(defaultLimitsTestConfig(), nil)
	require.NoError(t, err)

	store := &mockStore{
		chunks: map[string][]chunk.Chunk{},
	}

	i, err := New(ingesterConfig, client.Config{}, store, limits, runtime.DefaultTenantConfigs(), nil)
	require.NoError(t, err)
	defer services.StopAndAwaitTerminated(context.Background(), i) //nolint:errcheck

	req := logproto.PushRequest{
		Streams: []logproto.Stream{
			{
				Labels: `{foo="bar",bar="baz1"}`,
			},
			{
				Labels: `{foo="bar",bar="baz2"}`,
			},
		},
	}
	for i := 0; i < 10; i++ {
		req.Streams[0].Entries = append(req.Streams[0].Entries, logproto.Entry{
			Timestamp: time.Unix(0, 0),
			Line:      fmt.Sprintf("line %d", i),
		})
		req.Streams[1].Entries = append(req.Streams[1].Entries, logproto.Entry{
			Timestamp: time.Unix(0, 0),
			Line:      fmt.Sprintf("line %d", i),
		})
	}

	ctx := user.InjectOrgID(context.Background(), "test")
	_, err = i.Push(ctx, &req)
	require.NoError(t, err)

	res, err := i.Label(ctx, &logproto.LabelRequest{
		Name:   "bar",
		Values: true,
	})

	require.NoError(t, err)
	require.Equal(t, []string{"baz1", "baz2"}, res.Values)

	res, err = i.Label(ctx, &logproto.LabelRequest{})
	require.NoError(t, err)
	require.Equal(t, []string{"bar", "foo"}, res.Values)
}

func InMemoryLabels(t *testing.T, labelFilterer LabelValueFilterer, expectedValues []string) {
	ingesterConfig := defaultIngesterTestConfig(t)
	ingesterConfig.QueryStore = true
	limits, err := validation.NewOverrides(defaultLimitsTestConfig(), nil)
	require.NoError(t, err)

	store := &mockStore{
		chunks: map[string][]chunk.Chunk{},
	}

	i, err := New(ingesterConfig, client.Config{}, store, limits, runtime.DefaultTenantConfigs(), nil)
	require.NoError(t, err)
	i.labelFilter = labelFilterer
	future := time.Now().Local().Add(time.Minute * 5)
	i.periodicConfigs = []chunk.PeriodConfig{
		{
			From: chunk.DayTime{
				Time: model.TimeFromUnix(future.Unix()),
			},
		},
	}
	defer services.StopAndAwaitTerminated(context.Background(), i) //nolint:errcheck

	req := logproto.PushRequest{
		Streams: []logproto.Stream{
			{
				Labels: `{foo="bar",bar="baz1"}`,
			},
			{
				Labels: `{foo="bar",bar="baz2"}`,
			},
		},
	}
	for i := 0; i < 10; i++ {
		req.Streams[0].Entries = append(req.Streams[0].Entries, logproto.Entry{
			Timestamp: time.Unix(0, 0),
			Line:      fmt.Sprintf("line %d", i),
		})
		req.Streams[1].Entries = append(req.Streams[1].Entries, logproto.Entry{
			Timestamp: time.Unix(0, 0),
			Line:      fmt.Sprintf("line %d", i),
		})
	}

	ctx := user.InjectOrgID(context.Background(), "test")
	_, err = i.Push(ctx, &req)
	require.NoError(t, err)

	start := time.Now().Add(-5 * time.Minute)
	end := time.Now()
	res, err := i.Label(ctx, &logproto.LabelRequest{
		Name:   "bar",
		Values: true,
		Start:  &start,
		End:    &end,
	})

	require.NoError(t, err)
	require.Equal(t, expectedValues, res.Values)

	res, err = i.Label(ctx, &logproto.LabelRequest{})
	require.NoError(t, err)
	require.Equal(t, []string{"bar", "foo"}, res.Values)
}

func Test_InMemoryLabels_WithoutLabelFilter(t *testing.T) {
	expectedValues := []string{"baz1", "baz2", "val1", "val2"}

	InMemoryLabels(t, nil, expectedValues)
}

// DummyLabelValuedFilterer adds i to the front of the label value
type DummyLabelValueFilterer struct{}

func (*DummyLabelValueFilterer) Filter(ctx context.Context, labelName string, labelValues []string) ([]string, error) {
	var updatedValues []string

	for _, v := range labelValues {
		updatedValues = append(updatedValues, fmt.Sprintf("i%v", v))
	}

	return updatedValues, nil
}

func Test_InMemoryLabels_WithLabelFilter(t *testing.T) {
	labelFilter := DummyLabelValueFilterer{}
	expectedValues := []string{"ibaz1", "ibaz2", "ival1", "ival2"}

	InMemoryLabels(t, &labelFilter, expectedValues)
}
