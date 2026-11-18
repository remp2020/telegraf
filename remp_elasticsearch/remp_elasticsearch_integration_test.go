package remp_elasticsearch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/metric"
	"github.com/influxdata/telegraf/testutil"
)

// Same version as REMP's Docker/elasticsearch image.
const esImage = "docker.elastic.co/elasticsearch/elasticsearch:8.6.1"

func startElasticsearch(t *testing.T) string {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	container := testutil.Container{
		Image:        esImage,
		Quiet:        true,
		ExposedPorts: []string{"9200"},
		Env: map[string]string{
			"discovery.type":         "single-node",
			"xpack.security.enabled": "false",
			"ES_JAVA_OPTS":           "-Xms512m -Xmx512m",
		},
		WaitingFor: wait.ForHTTP("/_cluster/health?wait_for_status=yellow&timeout=60s").
			WithPort("9200").
			WithStartupTimeout(3 * time.Minute),
	}
	require.NoError(t, container.Start(), "failed to start container")
	t.Cleanup(container.Terminate)
	return fmt.Sprintf("http://%s:%s", container.Address, container.Ports["9200"])
}

func getDoc(t *testing.T, url, index, id string) map[string]any {
	resp, err := http.Get(url + "/" + index + "/_doc/" + id)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "document %s/%s", index, id)
	var body struct {
		Source map[string]any `json:"_source"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body.Source
}

func event(name, jsonBody string) telegraf.Metric {
	return metric.New(name, map[string]string{"host": "h1"}, map[string]any{"_json": jsonBody}, time.Now())
}

func TestIntegration(t *testing.T) {
	url := startElasticsearch(t)

	t.Run("index by id overwrites document", func(t *testing.T) {
		p := newPlugin(t, url, func(p *RempElasticsearch) {
			p.IndexName = "pageviews"
			p.IDField = "remp_pageview_id"
		})
		require.NoError(t, p.Write([]telegraf.Metric{event("pageviews", `{"remp_pageview_id":"p1","article_id":"a1","signed_in":true}`)}))
		require.NoError(t, p.Write([]telegraf.Metric{event("pageviews", `{"remp_pageview_id":"p1","article_id":"a2"}`)}))

		doc := getDoc(t, url, "pageviews", "p1")
		require.Equal(t, "a2", doc["article_id"])
		require.NotContains(t, doc, "signed_in")
		require.Equal(t, "h1", doc["host"])
	})

	t.Run("updated_fields upserts", func(t *testing.T) {
		p := newPlugin(t, url, func(p *RempElasticsearch) {
			p.IndexName = "pageviews_time_spent"
			p.IDField = "remp_pageview_id"
			p.UpdatedFields = []string{"timespent", "unload"}
			p.FieldWhitelist = []string{"token", "timespent", "unload"}
		})
		require.NoError(t, p.Write([]telegraf.Metric{
			event("pageviews_time_spent", `{"remp_pageview_id":"p1","token":"t1","timespent":10,"dropped":"x"}`),
		}))
		doc := getDoc(t, url, "pageviews_time_spent", "p1")
		require.Equal(t, map[string]any{"remp_pageview_id": "p1", "token": "t1", "timespent": 10.0, "time": doc["time"]}, doc)

		require.NoError(t, p.Write([]telegraf.Metric{
			event("pageviews_time_spent", `{"remp_pageview_id":"p1","token":"t2","timespent":25,"unload":true}`),
		}))
		doc = getDoc(t, url, "pageviews_time_spent", "p1")
		require.Equal(t, 25.0, doc["timespent"])
		require.Equal(t, true, doc["unload"])
		require.Equal(t, "t1", doc["token"], "fields outside updated_fields stay untouched")
	})

	t.Run("added_fields merges arrays", func(t *testing.T) {
		p := newPlugin(t, url, func(p *RempElasticsearch) {
			p.IndexName = "impressions"
			p.IDField = "remp_pageview_id_block_type"
			p.AddedFields = []string{"element_ids"}
		})
		require.NoError(t, p.Write([]telegraf.Metric{
			event("impressions", `{"remp_pageview_id_block_type":"p1_article","block":"article","element_ids":["e1","e2"]}`),
		}))
		// Two updates of the same document in one batch, one with a scalar.
		require.NoError(t, p.Write([]telegraf.Metric{
			event("impressions", `{"remp_pageview_id_block_type":"p1_article","block":"changed","element_ids":["e2","e3"]}`),
			event("impressions", `{"remp_pageview_id_block_type":"p1_article","element_ids":"e4"}`),
		}))
		doc := getDoc(t, url, "impressions", "p1_article")
		require.Equal(t, []any{"e1", "e2", "e3", "e4"}, doc["element_ids"])
		require.Equal(t, "article", doc["block"], "only added_fields change on existing documents")
	})

	t.Run("invalid documents are dropped, the rest is written", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPut, url+"/strict", strings.NewReader(
			`{"mappings":{"properties":{"timespent":{"type":"long"}}}}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		p := newPlugin(t, url, func(p *RempElasticsearch) {
			p.IndexName = "strict"
			p.IDField = "remp_pageview_id"
		})
		err = p.Write([]telegraf.Metric{
			event("pageviews", `{"remp_pageview_id":"ok","timespent":5}`),
			event("pageviews", `{"remp_pageview_id":"bad","timespent":"not a number"}`),
		})
		var pwe *internal.PartialWriteError
		require.ErrorAs(t, err, &pwe)
		require.Equal(t, []int{0}, pwe.MetricsAccept)
		require.Equal(t, []int{1}, pwe.MetricsReject)
		require.ErrorContains(t, pwe.MetricsRejectErrors[0], "mapper_parsing_exception")
		require.Equal(t, 5.0, getDoc(t, url, "strict", "ok")["timespent"])
	})

	t.Run("index-level failures are retried", func(t *testing.T) {
		// An alias over two indices without a write index fails every write
		// with status 400; that must not drop the data.
		for _, req := range []struct{ method, path, body string }{
			{http.MethodPut, "/alias-a", ""},
			{http.MethodPut, "/alias-b", ""},
			{http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"alias-a","alias":"noalias"}},{"add":{"index":"alias-b","alias":"noalias"}}]}`},
		} {
			r, err := http.NewRequest(req.method, url+req.path, strings.NewReader(req.body))
			require.NoError(t, err)
			r.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(r)
			require.NoError(t, err)
			resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode, req.path)
		}

		p := newPlugin(t, url, func(p *RempElasticsearch) {
			p.IndexName = "noalias"
			p.IDField = "remp_pageview_id"
		})
		err := p.Write([]telegraf.Metric{event("pageviews", `{"remp_pageview_id":"p1"}`)})
		var pwe *internal.PartialWriteError
		require.ErrorAs(t, err, &pwe)
		require.Empty(t, pwe.MetricsAccept)
		require.Empty(t, pwe.MetricsReject, "metric stays in the buffer")
		require.ErrorContains(t, err, "1 of 1 metrics will be retried")
	})
}
