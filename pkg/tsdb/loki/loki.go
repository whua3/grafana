package loki

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/grafana/grafana/pkg/components/null"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/tsdb"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/api"
	apiv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

type LokiExecutor struct {
	Transport http.RoundTripper
}

type basicAuthTransport struct {
	Transport http.RoundTripper

	username string
	password string
}

func (bat basicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.SetBasicAuth(bat.username, bat.password)
	return bat.Transport.RoundTrip(req)
}

func NewLokiExecutor(dsInfo *models.DataSource) (tsdb.TsdbQueryEndpoint, error) {
	transport, err := dsInfo.GetHttpTransport()
	if err != nil {
		return nil, err
	}

	return &LokiExecutor{
		Transport: transport,
	}, nil
}

var (
	plog               log.Logger
	legendFormat       *regexp.Regexp
	intervalCalculator tsdb.IntervalCalculator
)

func init() {
	plog = log.New("tsdb.loki")
	tsdb.RegisterTsdbQueryEndpoint("loki", NewLokiExecutor)
	legendFormat = regexp.MustCompile(`\{\{\s*(.+?)\s*\}\}`)
	intervalCalculator = tsdb.NewIntervalCalculator(&tsdb.IntervalOptions{MinInterval: time.Second * 1})
}

func (e *LokiExecutor) getClient(dsInfo *models.DataSource) (apiv1.API, error) {
	cfg := api.Config{
		Address:      dsInfo.Url + "/loki/",
		RoundTripper: e.Transport,
	}

	if dsInfo.BasicAuth {
		cfg.RoundTripper = basicAuthTransport{
			Transport: e.Transport,
			username:  dsInfo.BasicAuthUser,
			password:  dsInfo.DecryptedBasicAuthPassword(),
		}
	}

	client, err := api.NewClient(cfg)
	if err != nil {
		return nil, err
	}

	return apiv1.NewAPI(client), nil
}

func (e *LokiExecutor) Query(ctx context.Context, dsInfo *models.DataSource, tsdbQuery *tsdb.TsdbQuery) (*tsdb.Response, error) {
	result := &tsdb.Response{
		Results: map[string]*tsdb.QueryResult{},
	}

	client, err := e.getClient(dsInfo)
	if err != nil {
		return nil, err
	}

	queries, err := parseQuery(dsInfo, tsdbQuery.Queries, tsdbQuery)
	if err != nil {
		return nil, err
	}

	for _, query := range queries {
		timeRange := apiv1.Range{
			Start: query.Start,
			End:   query.End,
			Step:  query.Step,
		}

		plog.Debug("Sending query", "start", timeRange.Start, "end", timeRange.End, "step", timeRange.Step, "query", query.Expr)

		span, ctx := opentracing.StartSpanFromContext(ctx, "alerting.loki")
		span.SetTag("expr", query.Expr)
		span.SetTag("start_unixnano", query.Start.UnixNano())
		span.SetTag("stop_unixnano", query.End.UnixNano())
		defer span.Finish()

		value, _, err := client.QueryRange(ctx, query.Expr, timeRange)

		if err != nil {
			return nil, err
		}

		queryResult, err := parseResponse(value, query)
		if err != nil {
			return nil, err
		}
		result.Results[query.RefId] = queryResult
	}

	return result, nil
}

func formatLegend(metric model.Metric, query *LokiQuery) string {
	if query.LegendFormat == "" {
		return metric.String()
	}

	result := legendFormat.ReplaceAllFunc([]byte(query.LegendFormat), func(in []byte) []byte {
		labelName := strings.Replace(string(in), "{{", "", 1)
		labelName = strings.Replace(labelName, "}}", "", 1)
		labelName = strings.TrimSpace(labelName)
		if val, exists := metric[model.LabelName(labelName)]; exists {
			return []byte(val)
		}
		return []byte{}
	})

	return string(result)
}

func parseQuery(dsInfo *models.DataSource, queries []*tsdb.Query, queryContext *tsdb.TsdbQuery) ([]*LokiQuery, error) {
	qs := []*LokiQuery{}
	for _, queryModel := range queries {
		expr, err := queryModel.Model.Get("expr").String()
		if err != nil {
			return nil, err
		}

		format := queryModel.Model.Get("legendFormat").MustString("")

		start, err := queryContext.TimeRange.ParseFrom()
		if err != nil {
			return nil, err
		}

		end, err := queryContext.TimeRange.ParseTo()
		if err != nil {
			return nil, err
		}

		dsInterval, err := tsdb.GetIntervalFrom(dsInfo, queryModel.Model, time.Second)
		if err != nil {
			return nil, err
		}

		interval := intervalCalculator.Calculate(queryContext.TimeRange, dsInterval)
		step := time.Duration(int64(interval.Value))

		qs = append(qs, &LokiQuery{
			Expr:         expr,
			Step:         step,
			LegendFormat: format,
			Start:        start,
			End:          end,
			RefId:        queryModel.RefId,
		})
	}

	return qs, nil
}

func parseResponse(value model.Value, query *LokiQuery) (*tsdb.QueryResult, error) {
	queryRes := tsdb.NewQueryResult()

	data, ok := value.(model.Matrix)
	if !ok {
		return queryRes, fmt.Errorf("unsupported result format: %q", value.Type().String())
	}

	for _, v := range data {
		series := tsdb.TimeSeries{
			Name:   formatLegend(v.Metric, query),
			Tags:   make(map[string]string, len(v.Metric)),
			Points: make([]tsdb.TimePoint, 0, len(v.Values)),
		}

		for k, v := range v.Metric {
			series.Tags[string(k)] = string(v)
		}

		for _, k := range v.Values {
			series.Points = append(series.Points, tsdb.NewTimePoint(null.FloatFrom(float64(k.Value)), float64(k.Timestamp.Unix()*1000)))
		}

		queryRes.Series = append(queryRes.Series, &series)
	}

	return queryRes, nil
}

// IsAPIError returns whether err is or wraps a Loki error.
func IsAPIError(err error) bool {
	// Check if the right error type is in err's chain.
	var e *apiv1.Error
	return errors.As(err, &e)
}

func ConvertAPIError(err error) error {
	var e *apiv1.Error
	if errors.As(err, &e) {
		return fmt.Errorf("%s: %s", e.Msg, e.Detail)
	}
	return err
}