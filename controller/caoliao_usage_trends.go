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
	modelNames, ok := caoliaoCSV(c.Query("models"), 20, caoliaoUsageNamePattern)
	if !ok {
		caoliaoError(c, http.StatusBadRequest, "invalid models")
		return
	}
	if len(modelNames) == 0 {
		modelNames = []string{"deepseek-v4-flash", "qwen3.6"}
	}
	tokenIDs := make([]int, 0, len(tokens))
	for _, token := range tokens {
		tokenIDs = append(tokenIDs, token.Id)
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
	common.ApiSuccess(c, gin.H{
		"period": "daily", "timezone": "Asia/Shanghai",
		"start": startAt, "end": endAt, "summary": summary, "models": models,
	})
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
