package remp_elasticsearch

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/metric"
	"github.com/influxdata/telegraf/plugins/outputs"
	"github.com/influxdata/telegraf/testutil"
)

var ts = time.Date(2025, 6, 6, 10, 20, 30, 0, time.UTC)

// fakeES answers the version check and records bulk requests. itemStatus
// returns the status of the n-th bulk item (0 means 201).
type fakeES struct {
	*httptest.Server
	mu         sync.Mutex
	requests   [][]map[string]any
	itemStatus func(n int) int
	bulkStatus int
}

func newFakeES(t *testing.T) *fakeES {
	f := &fakeES{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`{"version":{"number":"8.6.1"}}`))
		case "/_bulk":
			lines := parseNDJSON(t, r.Body)
			f.mu.Lock()
			f.requests = append(f.requests, lines)
			itemStatus, bulkStatus := f.itemStatus, f.bulkStatus
			f.mu.Unlock()
			if bulkStatus != 0 {
				w.WriteHeader(bulkStatus)
				_, _ = w.Write([]byte(`{"error":{"type":"x","reason":"y"}}`))
				return
			}
			items := make([]map[string]any, 0, len(lines)/2)
			for n := 0; n < len(lines)/2; n++ {
				status := 201
				if itemStatus != nil && itemStatus(n) != 0 {
					status = itemStatus(n)
				}
				var op string
				for k := range lines[2*n] {
					op = k
				}
				item := map[string]any{"_id": "id", "status": status}
				if status >= 300 {
					item["error"] = map[string]any{"type": "mapper_parsing_exception", "reason": "failed to parse"}
				}
				items = append(items, map[string]any{op: item})
			}
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"errors": true, "items": items}))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeES) lastRequest() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

func parseNDJSON(t *testing.T, r io.Reader) []map[string]any {
	var lines []map[string]any
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for s.Scan() {
		if len(bytes.TrimSpace(s.Bytes())) == 0 {
			continue
		}
		var line map[string]any
		require.NoError(t, json.Unmarshal(s.Bytes(), &line))
		lines = append(lines, line)
	}
	require.NoError(t, s.Err())
	return lines
}

func newPlugin(t *testing.T, url string, modify func(*RempElasticsearch)) *RempElasticsearch {
	p := outputs.Outputs["remp_elasticsearch"]().(*RempElasticsearch)
	p.URLs = []string{url}
	p.IndexName = "pageviews"
	p.Log = testutil.Logger{}
	if modify != nil {
		modify(p)
	}
	require.NoError(t, p.Init())
	require.NoError(t, p.Connect())
	t.Cleanup(func() { require.NoError(t, p.Close()) })
	return p
}

func pageview(id string, fields map[string]any) telegraf.Metric {
	f := map[string]any{"_json": `{"remp_pageview_id":"` + id + `","article_id":"a1","timespent":10}`}
	for k, v := range fields {
		f[k] = v
	}
	return metric.New("pageviews", map[string]string{"host": "h1"}, f, ts)
}

func TestSampleConfig(t *testing.T) {
	cfg := config.NewConfig()
	require.NoError(t, cfg.LoadConfigData([]byte(sampleConfig), config.EmptySourcePath))
	require.Len(t, cfg.Outputs, 1)
}

// The production configuration of Denník N must keep loading; the
// manage_template option is only accepted for compatibility.
func TestProductionStyleConfigLoads(t *testing.T) {
	cfg := config.NewConfig()
	conf := `
[[outputs.remp_elasticsearch]]
  alias = "impressions"
  urls = ["https://localhost:9200"]
  timeout = "5s"
  index_name = "denniknsk-impressions"
  namepass = ["impressions"]
  added_fields = ["element_ids"]
  manage_template = false
  id_field = "remp_pageview_id_block_type"
  tagexclude = ["host"]
  username = "user"
  password = "secret"
  field_whitelist = ["token"]
  health_check_interval = "10s"
  enable_sniffer = false
`
	require.NoError(t, cfg.LoadConfigData([]byte(conf), config.EmptySourcePath))
	require.Len(t, cfg.Outputs, 1)
	p := cfg.Outputs[0].Output.(*RempElasticsearch)
	require.Equal(t, []string{"element_ids"}, p.AddedFields)
	require.Equal(t, "remp_pageview_id_block_type", p.IDField)

	// Removed options make Telegraf refuse the configuration.
	cfg = config.NewConfig()
	removed := "[[outputs.remp_elasticsearch]]\n  urls = [\"http://localhost:9200\"]\n  index_name = \"x\"\n  incremented_fields = [\"clicks\"]\n"
	require.ErrorContains(t, cfg.LoadConfigData([]byte(removed), config.EmptySourcePath), "incremented_fields")
}

func TestInitValidation(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*RempElasticsearch)
		err    string
	}{
		{"no urls", func(p *RempElasticsearch) { p.URLs = nil }, "no urls"},
		{"no index", func(p *RempElasticsearch) { p.IndexName = "" }, "index_name"},
		{"both modes", func(p *RempElasticsearch) {
			p.IDField = "id"
			p.UpdatedFields = []string{"x"}
			p.AddedFields = []string{"y"}
		}, "cannot be used together"},
		{"update without id", func(p *RempElasticsearch) { p.UpdatedFields = []string{"x"} }, "id_field is required"},
		{"add without id", func(p *RempElasticsearch) { p.AddedFields = []string{"x"} }, "id_field is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &RempElasticsearch{URLs: []string{"http://localhost:9200"}, IndexName: "i", Log: testutil.Logger{}}
			tt.modify(p)
			require.ErrorContains(t, p.Init(), tt.err)
		})
	}
}

func TestInitDeprecatedSSLOptions(t *testing.T) {
	p := &RempElasticsearch{
		URLs: []string{"http://localhost:9200"}, IndexName: "i", Log: testutil.Logger{},
		SSLCA: "ca.pem", SSLCert: "cert.pem", SSLKey: "key.pem",
	}
	p.TLSCert = "explicit.pem"
	require.NoError(t, p.Init())
	require.Equal(t, "ca.pem", p.TLSCA)
	require.Equal(t, "explicit.pem", p.TLSCert, "tls_* option wins over deprecated ssl_*")
	require.Equal(t, "key.pem", p.TLSKey)
}

func TestIndexName(t *testing.T) {
	m := metric.New("m", map[string]string{"host": "h1", "dc": "dc1"}, map[string]any{"v": 1}, time.Date(2014, 12, 1, 23, 30, 0, 0, time.UTC))
	tests := []struct{ pattern, expected string }{
		{"indexname", "indexname"},
		{"indexname-%Y", "indexname-2014"},
		{"indexname-%Y-%m-%d-%H", "indexname-2014-12-01-23"},
		{"indexname-%y-%m", "indexname-14-12"},
		{"indexname-%Y-%V", "indexname-2014-49"},
		{"indexname-{{host}}", "indexname-h1"},
		{"indexname-{{ host }}-{{dc}}-%Y", "indexname-h1-dc1-2014"},
		{"indexname-{{missing}}", "indexname-none"},
		{"{{host}}-{{host}}", "h1-h1"},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			p := &RempElasticsearch{URLs: []string{"x"}, IndexName: tt.pattern, DefaultTagValue: "none", Log: testutil.Logger{}}
			require.NoError(t, p.Init())
			require.Equal(t, tt.expected, p.indexName(m))
		})
	}
	// Non-UTC timestamps are converted to UTC.
	p := &RempElasticsearch{URLs: []string{"x"}, IndexName: "i-%d-%H", Log: testutil.Logger{}}
	require.NoError(t, p.Init())
	local := metric.New("m", nil, map[string]any{"v": 1}, time.Date(2014, 12, 2, 0, 30, 0, 0, time.FixedZone("CET", 3600)))
	require.Equal(t, "i-01-23", p.indexName(local))
}

func TestDocument(t *testing.T) {
	p := &RempElasticsearch{URLs: []string{"x"}, IndexName: "i", Log: testutil.Logger{}}
	require.NoError(t, p.Init())

	m := metric.New("pageviews",
		map[string]string{"host": "h1", "subscriber": "1", "locked": "0"},
		map[string]any{
			"_json":    `{"user_id":"u1","signed_in":true,"nested":{"a":[1,2]},"time":"overridden-by-json"}`,
			"count":    int64(3),
			"ratio":    0.5,
			"unsigned": uint64(7),
		},
		ts,
	)
	doc, err := p.document(m)
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"time":       "overridden-by-json",
		"host":       "h1",
		"subscriber": true,
		"locked":     false,
		"user_id":    "u1",
		"signed_in":  true,
		"nested":     map[string]any{"a": []any{1.0, 2.0}},
		"count":      int64(3),
		"ratio":      0.5,
		"unsigned":   uint64(7),
	}, doc)

	doc, err = p.document(metric.New("m", nil, map[string]any{"v": 1}, ts))
	require.NoError(t, err)
	require.Equal(t, "2025-06-06T10:20:30Z", doc["time"])
}

func TestDocumentWhitelist(t *testing.T) {
	p := &RempElasticsearch{URLs: []string{"x"}, IndexName: "i", FieldWhitelist: []string{"timespent"}, Log: testutil.Logger{}}
	require.NoError(t, p.Init())
	m := metric.New("pageviews", map[string]string{"host": "h1"}, map[string]any{
		"_json": `{"remp_pageview_id":"p1","browser_id":"b1","timespent":5,"token":"t"}`,
	}, ts)
	doc, err := p.document(m)
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"time":             "2025-06-06T10:20:30Z",
		"remp_pageview_id": "p1",
		"browser_id":       "b1",
		"timespent":        5.0,
	}, doc)
}

func TestDocumentInvalidJSON(t *testing.T) {
	p := &RempElasticsearch{URLs: []string{"x"}, IndexName: "i", Log: testutil.Logger{}}
	require.NoError(t, p.Init())
	_, err := p.document(metric.New("m", nil, map[string]any{"_json": `{"broken`}, ts))
	require.ErrorContains(t, err, "decoding field")
	_, err = p.document(metric.New("m", nil, map[string]any{"_json": int64(1)}, ts))
	require.ErrorContains(t, err, "invalid type")
}

func TestWriteIndex(t *testing.T) {
	es := newFakeES(t)
	p := newPlugin(t, es.URL, func(p *RempElasticsearch) { p.IDField = "remp_pageview_id" })

	require.NoError(t, p.Write([]telegraf.Metric{pageview("p1", nil), pageview("p2", nil)}))
	lines := es.lastRequest()
	require.Len(t, lines, 4)
	require.Equal(t, map[string]any{"index": map[string]any{"_index": "pageviews", "_id": "p1"}}, lines[0])
	require.Equal(t, map[string]any{
		"time": "2025-06-06T10:20:30Z", "host": "h1", "remp_pageview_id": "p1", "article_id": "a1", "timespent": 10.0,
	}, lines[1])
	require.Equal(t, map[string]any{"index": map[string]any{"_index": "pageviews", "_id": "p2"}}, lines[2])

	// Without id_field, Elasticsearch generates the ID.
	p = newPlugin(t, es.URL, nil)
	require.NoError(t, p.Write([]telegraf.Metric{pageview("p1", nil)}))
	require.Equal(t, map[string]any{"index": map[string]any{"_index": "pageviews"}}, es.lastRequest()[0])
}

func TestWriteUpdatedFields(t *testing.T) {
	es := newFakeES(t)
	p := newPlugin(t, es.URL, func(p *RempElasticsearch) {
		p.IDField = "remp_pageview_id"
		p.UpdatedFields = []string{"timespent", "unload"}
	})

	require.NoError(t, p.Write([]telegraf.Metric{pageview("p1", nil)}))
	lines := es.lastRequest()
	require.Len(t, lines, 2)
	require.Equal(t, map[string]any{"update": map[string]any{"_index": "pageviews", "_id": "p1", "retry_on_conflict": 3.0}}, lines[0])
	require.Equal(t, map[string]any{
		"script": map[string]any{
			"source": updateScript,
			"lang":   "painless",
			// Missing fields are set to null, like before.
			"params": map[string]any{"fields": map[string]any{"timespent": 10.0, "unload": nil}},
		},
		"upsert": map[string]any{
			"time": "2025-06-06T10:20:30Z", "host": "h1", "remp_pageview_id": "p1", "article_id": "a1", "timespent": 10.0,
		},
	}, lines[1])
}

func TestWriteAddedFields(t *testing.T) {
	es := newFakeES(t)
	p := newPlugin(t, es.URL, func(p *RempElasticsearch) {
		p.IndexName = "impressions"
		p.IDField = "remp_pageview_id_block_type"
		p.AddedFields = []string{"element_ids", "other"}
	})

	impression := metric.New("impressions", nil, map[string]any{
		"_json": `{"remp_pageview_id_block_type":"p1_article","element_ids":["e1","e2"]}`,
	}, ts)
	noValues := metric.New("impressions", nil, map[string]any{
		"_json": `{"remp_pageview_id_block_type":"p2_article"}`,
	}, ts)

	require.NoError(t, p.Write([]telegraf.Metric{impression, noValues}))
	lines := es.lastRequest()
	require.Len(t, lines, 2, "metric without any added field is skipped")
	require.Equal(t, map[string]any{"update": map[string]any{"_index": "impressions", "_id": "p1_article", "retry_on_conflict": 3.0}}, lines[0])
	require.Equal(t, map[string]any{
		"script": map[string]any{
			"source": addScript,
			"lang":   "painless",
			"params": map[string]any{"fields": map[string]any{"element_ids": []any{"e1", "e2"}}},
		},
		"upsert": map[string]any{
			"time": "2025-06-06T10:20:30Z", "remp_pageview_id_block_type": "p1_article", "element_ids": []any{"e1", "e2"},
		},
	}, lines[1])
}

func TestBadDocument(t *testing.T) {
	tests := []struct {
		status    int
		errType   string
		reason    string
		permanent bool
	}{
		{400, "mapper_parsing_exception", "failed to parse field [timespent]", true},
		{400, "document_parsing_exception", "failed to parse", true},
		{400, "illegal_argument_exception", "mapper [x] cannot be changed from type [long] to [text]", true},
		{400, "script_exception", "runtime error", true},
		{400, "", "", true},
		// Caused by the index, not the document: keep for retry.
		{400, "illegal_argument_exception", "no write index is defined for alias [al]", false},
		{400, "validation_exception", "this action would add [2] shards, but this cluster currently has [5]/[1] maximum normal shards open", false},
		{400, "invalid_index_name_exception", "Invalid index name [Bad*Name]", false},
		{403, "cluster_block_exception", "index [a1] blocked by: [FORBIDDEN/8/index write (api)]", false},
		{404, "index_not_found_exception", "no such index [missing]", false},
		{409, "version_conflict_engine_exception", "version conflict", false},
		{429, "es_rejected_execution_exception", "rejected execution", false},
		{503, "unavailable_shards_exception", "primary shard is not active", false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d %s", tt.status, tt.reason), func(t *testing.T) {
			res := bulkItemResp{Status: tt.status}
			if tt.errType != "" {
				res.Error = &bulkItemError{Type: tt.errType, Reason: tt.reason}
			}
			require.Equal(t, tt.permanent, badDocument(res))
		})
	}
}

func TestWriteFailures(t *testing.T) {
	es := newFakeES(t)
	// Bulk items p1..p6: ok, mapping error, too many requests, server error,
	// version conflict, index missing.
	es.itemStatus = func(n int) int { return []int{201, 400, 429, 503, 409, 404}[n] }
	p := newPlugin(t, es.URL, func(p *RempElasticsearch) { p.IDField = "remp_pageview_id" })

	metrics := []telegraf.Metric{
		pageview("p1", nil),
		metric.New("pageviews", nil, map[string]any{"_json": `{"article_id":"no-id"}`}, ts), // never sent
		pageview("p2", nil),
		metric.New("pageviews", nil, map[string]any{"_json": `{"broken`}, ts), // never sent
		pageview("p3", nil),
		pageview("p4", nil),
		pageview("p5", nil),
		pageview("p6", nil),
		pageview("p7", map[string]any{"bad": math.NaN()}), // can't be encoded, never sent
	}
	err := p.Write(metrics)

	var pwe *internal.PartialWriteError
	require.ErrorAs(t, err, &pwe)
	require.ElementsMatch(t, []int{0}, pwe.MetricsAccept)
	require.ElementsMatch(t, []int{1, 2, 3, 8}, pwe.MetricsReject)
	require.Len(t, pwe.MetricsRejectErrors, 4)
	require.EqualError(t, err, "4 of 9 metrics dropped; 4 of 9 metrics will be retried")
	require.Len(t, es.lastRequest(), 12, "only valid metrics are sent")
}

func TestWriteOutcome(t *testing.T) {
	tests := []struct {
		name     string
		statuses []int
		metrics  []telegraf.Metric
		accept   []int
		reject   []int
	}{
		{"all written", []int{201, 200}, []telegraf.Metric{pageview("p1", nil), pageview("p2", nil)}, nil, nil},
		{"only temporary failures", []int{201, 429}, []telegraf.Metric{pageview("p1", nil), pageview("p2", nil)}, []int{0}, nil},
		{"only local rejects", nil, []telegraf.Metric{metric.New("pageviews", nil, map[string]any{"x": 1}, ts)}, nil, []int{0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			es := newFakeES(t)
			es.itemStatus = func(n int) int { return tt.statuses[n] }
			p := newPlugin(t, es.URL, func(p *RempElasticsearch) { p.IDField = "remp_pageview_id" })

			err := p.Write(tt.metrics)
			if tt.accept == nil && tt.reject == nil {
				require.NoError(t, err)
				return
			}
			var pwe *internal.PartialWriteError
			require.ErrorAs(t, err, &pwe)
			require.Equal(t, tt.accept, pwe.MetricsAccept)
			require.Equal(t, tt.reject, pwe.MetricsReject)
		})
	}
}

func TestWriteRequestFailureKeepsMetrics(t *testing.T) {
	es := newFakeES(t)
	p := newPlugin(t, es.URL, func(p *RempElasticsearch) { p.IDField = "remp_pageview_id" })

	es.bulkStatus = http.StatusServiceUnavailable
	err := p.Write([]telegraf.Metric{pageview("p1", nil)})
	require.Error(t, err)
	var pwe *internal.PartialWriteError
	require.False(t, errors.As(err, &pwe), "a failed request must keep all metrics for retry")

	// Locally rejected metrics are still dropped when the request fails.
	err = p.Write([]telegraf.Metric{pageview("p1", nil), metric.New("pageviews", nil, map[string]any{"x": 1}, ts)})
	require.ErrorAs(t, err, &pwe)
	require.Empty(t, pwe.MetricsAccept)
	require.Equal(t, []int{1}, pwe.MetricsReject)
}

func TestConnectFailsWithoutServer(t *testing.T) {
	p := &RempElasticsearch{
		URLs:      []string{"http://127.0.0.1:1"},
		IndexName: "i",
		Timeout:   config.Duration(time.Second),
		Log:       testutil.Logger{},
	}
	require.NoError(t, p.Init())
	require.Error(t, p.Connect())
}
