package controller

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

var caoliaoUsageNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var caoliaoMockDatasetPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type caoliaoUsageTrendPoint struct {
	Date string `json:"date"`
	caoliaoUsageMetrics
}

type caoliaoUsageTrendModel struct {
	ModelID string                   `json:"model_id"`
	Summary caoliaoUsageMetrics      `json:"summary"`
	Daily   []caoliaoUsageTrendPoint `json:"daily"`
}

// caoliaoUsageTokenBreakdown splits one token's usage by model so the control
// plane can show "this key spent this much on that model". It is additive and
// only emitted when the caller explicitly asks for it.
type caoliaoUsageTokenBreakdown struct {
	TokenId int    `json:"token_id"`
	ModelId string `json:"model_id"`
	caoliaoUsageMetrics
}

// caoliaoUsageChannelSeries is the channel-dimension counterpart of
// caoliaoUsageTrendModel. Only channel_id identifies a series: model names are
// informational and may change over the queried window without splitting the
// series.
type caoliaoUsageChannelSeries struct {
	SeriesID    string                   `json:"series_id"`
	ChannelID   int                      `json:"channel_id"`
	ChannelName string                   `json:"channel_name"`
	Status      int                      `json:"status"`
	ModelNames  []string                 `json:"model_names"`
	LatestModel string                   `json:"latest_model"`
	Summary     caoliaoUsageMetrics      `json:"summary"`
	Daily       []caoliaoUsageTrendPoint `json:"daily"`
}

type caoliaoUsageTokenChannelBreakdown struct {
	TokenId   int    `json:"token_id"`
	SeriesID  string `json:"series_id"`
	ChannelID int    `json:"channel_id"`
	caoliaoUsageMetrics
}

type caoliaoMockUsageRequest struct {
	Dataset string                 `json:"dataset"`
	Replace bool                   `json:"replace"`
	Items   []caoliaoMockUsageItem `json:"items"`
}

type caoliaoMockUsageItem struct {
	TokenID      int    `json:"token_id"`
	ModelID      string `json:"model_id"`
	OccurredAt   string `json:"occurred_at"`
	Successes    int    `json:"successes"`
	Failures     int    `json:"failures"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

func caoliaoCSV(raw string, maximum int, pattern *regexp.Regexp) ([]string, bool) {
	seen := map[string]bool{}
	values := make([]string, 0)
	for _, part := range strings.Split(raw, ",") {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		if pattern != nil && !pattern.MatchString(value) {
			return nil, false
		}
		if !seen[value] {
			seen[value] = true
			values = append(values, value)
		}
		if len(values) > maximum {
			return nil, false
		}
	}
	return values, true
}

func filterCaoliaoUsageTokens(c *gin.Context, tokens []*model.Token) ([]*model.Token, bool) {
	raw := strings.TrimSpace(c.Query("token_ids"))
	if single := strings.TrimSpace(c.Query("token_id")); single != "" {
		if raw != "" {
			raw += ","
		}
		raw += single
	}
	strict := strings.TrimSpace(c.Query("token_id")) != ""
	if raw == "" {
		return tokens, true
	}
	values, ok := caoliaoCSV(raw, 200, regexp.MustCompile(`^[0-9]+$`))
	if !ok || len(values) == 0 {
		caoliaoError(c, http.StatusBadRequest, "invalid token_ids")
		return nil, false
	}
	wanted := make(map[int]bool, len(values))
	for _, value := range values {
		id, err := strconv.Atoi(value)
		if err != nil || id <= 0 {
			caoliaoError(c, http.StatusBadRequest, "invalid token_ids")
			return nil, false
		}
		wanted[id] = true
	}
	filtered := make([]*model.Token, 0, len(wanted))
	for _, token := range tokens {
		if wanted[token.Id] {
			filtered = append(filtered, token)
			delete(wanted, token.Id)
		}
	}
	if strict && len(wanted) != 0 {
		caoliaoError(c, http.StatusNotFound, "key not found")
		return nil, false
	}
	return filtered, true
}

func getCaoliaoTrendUsage(c *gin.Context, tokens []*model.Token, startAt int64, endAt int64) {
	// dimension selects the grouping identity. "model" keeps the historical
	// behaviour untouched for the employee portal; "channel" groups by
	// channel_id so a channel rename cannot split or hide its history.
	dimension := strings.TrimSpace(c.Query("dimension"))
	if dimension != "" && dimension != "channel" {
		caoliaoError(c, http.StatusBadRequest, "invalid dimension")
		return
	}
	// Allow-listed on purpose: a typo must fail loudly instead of silently
	// omitting a section the caller believes it asked for.
	include := strings.TrimSpace(c.Query("include"))
	if include != "" && include != "tokens" {
		caoliaoError(c, http.StatusBadRequest, "invalid include")
		return
	}
	wantTokens := include == "tokens"
	tokenIDs := make([]int, 0, len(tokens))
	for _, token := range tokens {
		tokenIDs = append(tokenIDs, token.Id)
	}
	if dimension == "channel" {
		getCaoliaoChannelTrendUsage(c, tokenIDs, startAt, endAt, wantTokens)
		return
	}
	modelNames, ok := caoliaoCSV(c.Query("models"), 20, caoliaoUsageNamePattern)
	if !ok {
		caoliaoError(c, http.StatusBadRequest, "invalid models")
		return
	}
	if len(modelNames) == 0 {
		modelNames = []string{"deepseek-v4-flash", "qwen3.6"}
	}
	rows, err := model.QueryCaoliaoDailyUsage(tokenIDs, modelNames, startAt, endAt)
	if err != nil {
		caoliaoInternalError(c, err)
		return
	}
	byModel := make(map[string][]caoliaoUsageTrendPoint, len(modelNames))
	modelSummaries := make(map[string]caoliaoUsageMetrics, len(modelNames))
	summary := caoliaoUsageMetrics{}
	for _, row := range rows {
		requests := row.SuccessfulRequests + row.FailedRequests
		metrics := caoliaoUsageMetrics{
			Requests:        requests,
			InputTokens:     row.InputTokens,
			OutputTokens:    row.OutputTokens,
			ChargedTokens:   row.ChargedTokens,
			Successes:       row.SuccessfulRequests,
			Failures:        row.FailedRequests,
			RateLimitErrors: row.RateLimitErrors,
		}
		byModel[row.ModelName] = append(byModel[row.ModelName], caoliaoUsageTrendPoint{
			Date: row.UsageDay, caoliaoUsageMetrics: metrics,
		})
		current := modelSummaries[row.ModelName]
		addCaoliaoUsageMetrics(&current, metrics)
		modelSummaries[row.ModelName] = current
		addCaoliaoUsageMetrics(&summary, metrics)
	}
	models := make([]caoliaoUsageTrendModel, 0, len(modelNames))
	for _, modelName := range modelNames {
		points := byModel[modelName]
		if points == nil {
			points = []caoliaoUsageTrendPoint{}
		}
		sort.Slice(points, func(i, j int) bool { return points[i].Date < points[j].Date })
		models = append(models, caoliaoUsageTrendModel{
			ModelID: modelName, Summary: modelSummaries[modelName], Daily: points,
		})
	}
	payload := gin.H{
		"period": "daily", "timezone": "Asia/Shanghai",
		"start": startAt, "end": endAt, "summary": summary, "models": models,
	}
	if wantTokens {
		breakdown, err := model.QueryCaoliaoTokenModelUsage(tokenIDs, modelNames, startAt, endAt)
		if err != nil {
			caoliaoInternalError(c, err)
			return
		}
		rows := make([]caoliaoUsageTokenBreakdown, 0, len(breakdown))
		for _, row := range breakdown {
			rows = append(rows, caoliaoUsageTokenBreakdown{
				TokenId: row.TokenId, ModelId: row.ModelName,
				caoliaoUsageMetrics: caoliaoUsageMetrics{
					Requests:        row.SuccessfulRequests + row.FailedRequests,
					InputTokens:     row.InputTokens,
					OutputTokens:    row.OutputTokens,
					ChargedTokens:   row.ChargedTokens,
					Successes:       row.SuccessfulRequests,
					Failures:        row.FailedRequests,
					RateLimitErrors: row.RateLimitErrors,
				},
			})
		}
		payload["tokens"] = rows
	}
	common.ApiSuccess(c, payload)
}

// getCaoliaoChannelTrendUsage builds one series per channel. The query returns
// one row per (channel, model, day); rows are merged here rather than in SQL so
// the statement stays free of dialect-specific aggregation.
func getCaoliaoChannelTrendUsage(c *gin.Context, tokenIDs []int, startAt int64, endAt int64, wantTokens bool) {
	rows, err := model.QueryCaoliaoDailyUsageByChannel(tokenIDs, startAt, endAt)
	if err != nil {
		caoliaoInternalError(c, err)
		return
	}

	type channelAggregate struct {
		daily       map[string]caoliaoUsageMetrics
		summary     caoliaoUsageMetrics
		modelNames  map[string]bool
		latestDay   string
		latestModel string
	}
	aggregates := make(map[int]*channelAggregate)
	channelIDs := make([]int, 0)
	summary := caoliaoUsageMetrics{}
	for _, row := range rows {
		metrics := caoliaoUsageMetrics{
			Requests:        row.SuccessfulRequests + row.FailedRequests,
			InputTokens:     row.InputTokens,
			OutputTokens:    row.OutputTokens,
			ChargedTokens:   row.ChargedTokens,
			Successes:       row.SuccessfulRequests,
			Failures:        row.FailedRequests,
			RateLimitErrors: row.RateLimitErrors,
		}
		aggregate, ok := aggregates[row.ChannelId]
		if !ok {
			aggregate = &channelAggregate{
				daily:      make(map[string]caoliaoUsageMetrics),
				modelNames: make(map[string]bool),
			}
			aggregates[row.ChannelId] = aggregate
			channelIDs = append(channelIDs, row.ChannelId)
		}
		point := aggregate.daily[row.UsageDay]
		addCaoliaoUsageMetrics(&point, metrics)
		aggregate.daily[row.UsageDay] = point
		addCaoliaoUsageMetrics(&aggregate.summary, metrics)
		if row.ModelName != "" {
			aggregate.modelNames[row.ModelName] = true
		}
		// Rows arrive ordered by usage_day, so the last model seen on the newest
		// day is the one the channel was running at the end of the window.
		if row.UsageDay >= aggregate.latestDay {
			aggregate.latestDay = row.UsageDay
			aggregate.latestModel = row.ModelName
		}
		addCaoliaoUsageMetrics(&summary, metrics)
	}

	info, err := model.GetCaoliaoChannelInfo(channelIDs)
	if err != nil {
		caoliaoInternalError(c, err)
		return
	}

	sort.Ints(channelIDs)
	series := make([]caoliaoUsageChannelSeries, 0, len(channelIDs))
	for _, channelID := range channelIDs {
		aggregate := aggregates[channelID]
		days := make([]string, 0, len(aggregate.daily))
		for day := range aggregate.daily {
			days = append(days, day)
		}
		sort.Strings(days)
		points := make([]caoliaoUsageTrendPoint, 0, len(days))
		for _, day := range days {
			points = append(points, caoliaoUsageTrendPoint{
				Date: day, caoliaoUsageMetrics: aggregate.daily[day],
			})
		}
		names := make([]string, 0, len(aggregate.modelNames))
		for name := range aggregate.modelNames {
			names = append(names, name)
		}
		sort.Strings(names)
		channelName := ""
		status := 0
		if record, found := info[channelID]; found {
			channelName = record.Name
			status = record.Status
		}
		series = append(series, caoliaoUsageChannelSeries{
			SeriesID:    "channel:" + strconv.Itoa(channelID),
			ChannelID:   channelID,
			ChannelName: channelName,
			Status:      status,
			ModelNames:  names,
			LatestModel: aggregate.latestModel,
			Summary:     aggregate.summary,
			Daily:       points,
		})
	}

	payload := gin.H{
		"period": "daily", "timezone": "Asia/Shanghai", "dimension": "channel",
		"start": startAt, "end": endAt, "summary": summary, "series": series,
	}
	if wantTokens {
		breakdown, err := model.QueryCaoliaoTokenChannelUsage(tokenIDs, startAt, endAt)
		if err != nil {
			caoliaoInternalError(c, err)
			return
		}
		tokenRows := make([]caoliaoUsageTokenChannelBreakdown, 0, len(breakdown))
		for _, row := range breakdown {
			tokenRows = append(tokenRows, caoliaoUsageTokenChannelBreakdown{
				TokenId:   row.TokenId,
				SeriesID:  "channel:" + strconv.Itoa(row.ChannelId),
				ChannelID: row.ChannelId,
				caoliaoUsageMetrics: caoliaoUsageMetrics{
					Requests:        row.SuccessfulRequests + row.FailedRequests,
					InputTokens:     row.InputTokens,
					OutputTokens:    row.OutputTokens,
					ChargedTokens:   row.ChargedTokens,
					Successes:       row.SuccessfulRequests,
					Failures:        row.FailedRequests,
					RateLimitErrors: row.RateLimitErrors,
				},
			})
		}
		payload["tokens"] = tokenRows
	}
	common.ApiSuccess(c, payload)
}

func addCaoliaoUsageMetrics(total *caoliaoUsageMetrics, item caoliaoUsageMetrics) {
	total.Requests += item.Requests
	total.InputTokens += item.InputTokens
	total.OutputTokens += item.OutputTokens
	total.ChargedTokens += item.ChargedTokens
	total.Successes += item.Successes
	total.Failures += item.Failures
	total.RateLimitErrors += item.RateLimitErrors
}

// PostCaoliaoMockUsage is a local-development aid. It is absent unless the
// deployment explicitly enables it, and it remains protected by the private
// Caoliao machine credential applied to the route group.
func PostCaoliaoMockUsage(c *gin.Context) {
	if !common.GetEnvOrDefaultBool("CAOLIAO_ENABLE_MOCK_USAGE", false) {
		caoliaoError(c, http.StatusNotFound, "not found")
		return
	}
	request := caoliaoMockUsageRequest{}
	if err := c.ShouldBindJSON(&request); err != nil {
		caoliaoError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	request.Dataset = strings.TrimSpace(request.Dataset)
	if !caoliaoMockDatasetPattern.MatchString(request.Dataset) {
		caoliaoError(c, http.StatusBadRequest, "invalid dataset")
		return
	}
	if len(request.Items) == 0 || len(request.Items) > 1000 {
		caoliaoError(c, http.StatusBadRequest, "items must contain 1 to 1000 entries")
		return
	}
	marker := "caoliao_mock_usage:" + request.Dataset
	logs := make([]model.Log, 0)
	for _, item := range request.Items {
		if item.TokenID <= 0 || !caoliaoUsageNamePattern.MatchString(strings.TrimSpace(item.ModelID)) {
			caoliaoError(c, http.StatusBadRequest, "invalid token_id or model_id")
			return
		}
		token, err := model.GetCaoliaoManagedToken(item.TokenID)
		if err != nil {
			caoliaoError(c, http.StatusNotFound, "key not found")
			return
		}
		occurredAt, err := parseCaoliaoUsageTime(strings.TrimSpace(item.OccurredAt), false)
		if err != nil || occurredAt <= 0 || item.Successes < 0 || item.Successes > 500 ||
			item.Failures < 0 || item.Failures > 500 || item.Successes+item.Failures == 0 ||
			item.InputTokens < 0 || item.InputTokens > common.MaxQuota ||
			item.OutputTokens < 0 || item.OutputTokens > common.MaxQuota ||
			item.InputTokens > common.MaxQuota-item.OutputTokens ||
			(item.Successes == 0 && (item.InputTokens != 0 || item.OutputTokens != 0)) {
			caoliaoError(c, http.StatusBadRequest, "invalid usage item")
			return
		}
		if len(logs)+item.Successes+item.Failures > 10000 {
			caoliaoError(c, http.StatusBadRequest, "expanded request count exceeds 10000")
			return
		}
		for index := 0; index < item.Successes; index++ {
			inputTokens, outputTokens := 0, 0
			if index == 0 {
				inputTokens, outputTokens = item.InputTokens, item.OutputTokens
			}
			logs = append(logs, model.Log{
				UserId: token.UserId, CreatedAt: occurredAt + int64(index), Type: model.LogTypeConsume,
				Content: marker, TokenName: token.Name, ModelName: item.ModelID,
				Quota: inputTokens + outputTokens, PromptTokens: inputTokens,
				CompletionTokens: outputTokens, UseTime: 1 + index%4, TokenId: token.Id,
			})
		}
		for index := 0; index < item.Failures; index++ {
			logs = append(logs, model.Log{
				UserId: token.UserId, CreatedAt: occurredAt + int64(item.Successes+index),
				Type: model.LogTypeError, Content: marker, TokenName: token.Name,
				ModelName: item.ModelID, UseTime: 1, TokenId: token.Id,
			})
		}
	}
	deleted, inserted, err := model.ReplaceCaoliaoMockUsage(request.Dataset, logs, request.Replace)
	if err != nil {
		caoliaoInternalError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{
		"dataset": request.Dataset, "deleted": deleted, "inserted": inserted,
	})
}
