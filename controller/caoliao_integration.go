package controller

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	defaultCaoliaoTokenQuota = -1
	maxCaoliaoUsageRange     = 366 * 24 * time.Hour
)

var caoliaoUsageLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

type caoliaoEmployeeRequest struct {
	Name       string `json:"name"`
	Department string `json:"department"`
}

type caoliaoCreateKeyRequest struct {
	Name                string `json:"name"`
	TokenQuota          *int   `json:"token_quota"`
	RequestsPerTwoHours *int   `json:"requests_per_two_hours"`
	TokensPerTwoHours   *int   `json:"tokens_per_two_hours"`
	DailyTokenQuota     *int   `json:"daily_token_quota"`
	ExpiresAt           *int64 `json:"expires_at"`
}

type caoliaoUpdateKeyRequest struct {
	Name                *string `json:"name"`
	TokenQuota          *int    `json:"token_quota"`
	RequestsPerTwoHours *int    `json:"requests_per_two_hours"`
	TokensPerTwoHours   *int    `json:"tokens_per_two_hours"`
	DailyTokenQuota     *int    `json:"daily_token_quota"`
	ExpiresAt           *int64  `json:"expires_at"`
	Status              *string `json:"status"`
}

type caoliaoKeyMetadata struct {
	Id                  int    `json:"id"`
	Name                string `json:"name"`
	KeyMask             string `json:"key_mask"`
	Status              string `json:"status"`
	TokenQuota          int64  `json:"token_quota"`
	UsedQuota           int    `json:"used_quota"`
	RequestsPerTwoHours int    `json:"requests_per_two_hours"`
	TokensPerTwoHours   int    `json:"tokens_per_two_hours"`
	DailyTokenQuota     int    `json:"daily_token_quota"`
	ExpiresAt           int64  `json:"expires_at"`
	CreatedAt           int64  `json:"created_at"`
	LastUsedAt          int64  `json:"last_used_at"`
}

type caoliaoUsageMetrics struct {
	Requests        int64   `json:"requests"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	ChargedTokens   int64   `json:"charged_tokens"`
	Successes       int64   `json:"successes"`
	Failures        int64   `json:"failures"`
	LatencyMs       float64 `json:"latency_ms"`
	RateLimitErrors int64   `json:"rate_limit_errors"`
}

type caoliaoUsageItem struct {
	TokenId    int    `json:"token_id"`
	TokenName  string `json:"token_name"`
	EmployeeId string `json:"employee_id"`
	Period     string `json:"period"`
	Start      int64  `json:"start"`
	End        int64  `json:"end"`
	caoliaoUsageMetrics
}

func GetCaoliaoHealth(c *gin.Context) {
	count, err := model.GetCaoliaoIntegrationHealth(c.Request.Context())
	if err != nil {
		caoliaoInternalError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{
		"status":            "ok",
		"configured":        true,
		"database":          "ok",
		"log_database":      "ok",
		"managed_key_count": count,
		"checked_at":        common.GetTimestamp(),
	})
}

func PutCaoliaoEmployee(c *gin.Context) {
	employeeID, ok := caoliaoEmployeeID(c)
	if !ok {
		return
	}
	request := caoliaoEmployeeRequest{}
	if err := c.ShouldBindJSON(&request); err != nil {
		caoliaoError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.Department = strings.TrimSpace(request.Department)
	if request.Name == "" || request.Department == "" {
		caoliaoError(c, http.StatusBadRequest, "name and department are required")
		return
	}
	if len([]rune(request.Name)) > 100 || len([]rune(request.Department)) > 100 {
		caoliaoError(c, http.StatusBadRequest, "name or department is too long")
		return
	}
	user, err := model.EnsureCaoliaoEmployee(employeeID, request.Name)
	if err != nil {
		caoliaoInternalError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"subject_id": user.Id})
}

func GetCaoliaoEmployeeKeys(c *gin.Context) {
	employeeID, ok := caoliaoEmployeeID(c)
	if !ok {
		return
	}
	tokens, err := model.ListCaoliaoTokensByEmployee(employeeID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		caoliaoError(c, http.StatusNotFound, "employee not found")
		return
	}
	if err != nil {
		caoliaoInternalError(c, err)
		return
	}
	items := make([]caoliaoKeyMetadata, 0, len(tokens))
	for _, token := range tokens {
		items = append(items, buildCaoliaoKeyMetadata(token))
	}
	common.ApiSuccess(c, gin.H{"items": items})
}

func PostCaoliaoEmployeeKey(c *gin.Context) {
	employeeID, ok := caoliaoEmployeeID(c)
	if !ok {
		return
	}
	user, err := model.GetCaoliaoEmployee(employeeID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		caoliaoError(c, http.StatusNotFound, "employee not found")
		return
	}
	if err != nil {
		caoliaoInternalError(c, err)
		return
	}
	request := caoliaoCreateKeyRequest{}
	if err = c.ShouldBindJSON(&request); err != nil {
		caoliaoError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || len([]rune(request.Name)) > 50 {
		caoliaoError(c, http.StatusBadRequest, "key name is required and must not exceed 50 characters")
		return
	}

	quota := defaultCaoliaoTokenQuota
	if request.TokenQuota != nil {
		quota = *request.TokenQuota
	}
	requestsPerTwoHours := model.DefaultCaoliaoRequestsPerTwoHours
	if request.RequestsPerTwoHours != nil {
		requestsPerTwoHours = *request.RequestsPerTwoHours
	}
	tokensPerTwoHours := model.DefaultCaoliaoOutputTokensPerTwoHours
	if request.TokensPerTwoHours != nil {
		tokensPerTwoHours = *request.TokensPerTwoHours
	}
	dailyTokenQuota := model.DefaultCaoliaoDailyOutputTokenQuota
	if request.DailyTokenQuota != nil {
		dailyTokenQuota = *request.DailyTokenQuota
	}
	expiresAt := time.Now().Add(365 * 24 * time.Hour).Unix()
	if request.ExpiresAt != nil {
		expiresAt = *request.ExpiresAt
	}
	if message := validateCaoliaoKeyLimits(quota, requestsPerTwoHours, tokensPerTwoHours, dailyTokenQuota, expiresAt); message != "" {
		caoliaoError(c, http.StatusBadRequest, message)
		return
	}

	token, secret, err := model.CreateCaoliaoToken(user.Id, request.Name, quota, requestsPerTwoHours, tokensPerTwoHours, dailyTokenQuota, expiresAt)
	if err != nil {
		caoliaoInternalError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{
		"key":    buildCaoliaoKeyMetadata(token),
		"secret": secret,
	})
}

func PatchCaoliaoKey(c *gin.Context) {
	token, ok := caoliaoManagedToken(c)
	if !ok {
		return
	}
	request := caoliaoUpdateKeyRequest{}
	if err := c.ShouldBindJSON(&request); err != nil {
		caoliaoError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if request.Name == nil && request.TokenQuota == nil && request.RequestsPerTwoHours == nil && request.TokensPerTwoHours == nil && request.DailyTokenQuota == nil && request.ExpiresAt == nil && request.Status == nil {
		caoliaoError(c, http.StatusBadRequest, "at least one field must be provided")
		return
	}

	if request.Name != nil {
		name := strings.TrimSpace(*request.Name)
		if name == "" || len([]rune(name)) > 50 {
			caoliaoError(c, http.StatusBadRequest, "key name must not exceed 50 characters")
			return
		}
		token.Name = name
	}
	if request.TokenQuota != nil {
		if *request.TokenQuota != -1 && (*request.TokenQuota <= 0 || *request.TokenQuota > common.MaxQuota) {
			caoliaoError(c, http.StatusBadRequest, "token_quota is out of range")
			return
		}
		if *request.TokenQuota == -1 {
			token.UnlimitedQuota = true
			if token.Status == common.TokenStatusExhausted {
				token.Status = common.TokenStatusEnabled
			}
		} else {
			token.UnlimitedQuota = false
			remaining := int64(*request.TokenQuota) - int64(token.UsedQuota)
			if remaining < 0 {
				remaining = 0
			}
			token.RemainQuota = int(remaining)
			if token.Status == common.TokenStatusExhausted && remaining > 0 {
				token.Status = common.TokenStatusEnabled
			}
		}
	}
	requestsPerTwoHours := request.RequestsPerTwoHours
	if requestsPerTwoHours != nil {
		if *requestsPerTwoHours < 0 || *requestsPerTwoHours > common.MaxQuota {
			caoliaoError(c, http.StatusBadRequest, "requests_per_two_hours is out of range")
			return
		}
		token.RequestsPerTwoHours = *requestsPerTwoHours
	}
	tokensPerTwoHours := request.TokensPerTwoHours
	if tokensPerTwoHours != nil {
		if *tokensPerTwoHours < 0 || *tokensPerTwoHours > common.MaxQuota {
			caoliaoError(c, http.StatusBadRequest, "tokens_per_two_hours is out of range")
			return
		}
		token.TokensPerTwoHours = *tokensPerTwoHours
	}
	dailyTokenQuota := request.DailyTokenQuota
	if dailyTokenQuota != nil {
		if *dailyTokenQuota < 0 || *dailyTokenQuota > common.MaxQuota {
			caoliaoError(c, http.StatusBadRequest, "daily_token_quota is out of range")
			return
		}
		token.DailyTokenQuota = *dailyTokenQuota
	}
	if request.ExpiresAt != nil {
		if *request.ExpiresAt != -1 && *request.ExpiresAt <= common.GetTimestamp() {
			caoliaoError(c, http.StatusBadRequest, "expires_at must be in the future or -1")
			return
		}
		wasExpiredAndEnabled := token.Status == common.TokenStatusExpired ||
			(token.Status == common.TokenStatusEnabled && token.ExpiredTime != -1 && token.ExpiredTime <= common.GetTimestamp())
		token.ExpiredTime = *request.ExpiresAt
		if wasExpiredAndEnabled && (token.UnlimitedQuota || token.RemainQuota > 0) {
			token.Status = common.TokenStatusEnabled
		}
	}
	if request.Status != nil {
		switch strings.ToLower(strings.TrimSpace(*request.Status)) {
		case "active":
			if token.ExpiredTime != -1 && token.ExpiredTime <= common.GetTimestamp() {
				caoliaoError(c, http.StatusConflict, "expired key cannot be activated")
				return
			}
			if !token.UnlimitedQuota && token.RemainQuota <= 0 {
				caoliaoError(c, http.StatusConflict, "exhausted key cannot be activated")
				return
			}
			token.Status = common.TokenStatusEnabled
		case "disabled":
			token.Status = common.TokenStatusDisabled
		default:
			caoliaoError(c, http.StatusBadRequest, "status must be active or disabled")
			return
		}
	}
	if err := model.UpdateCaoliaoManagedToken(token); err != nil {
		caoliaoInternalError(c, err)
		return
	}
	common.ApiSuccess(c, buildCaoliaoKeyMetadata(token))
}

func DeleteCaoliaoKey(c *gin.Context) {
	token, ok := caoliaoManagedToken(c)
	if !ok {
		return
	}
	if err := model.DeleteCaoliaoManagedToken(token); err != nil {
		caoliaoInternalError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

func GetCaoliaoUsage(c *gin.Context) {
	startAt, endAt, ok := caoliaoUsageRange(c)
	if !ok {
		return
	}
	employeeID := strings.TrimSpace(c.Query("employee_id"))
	var tokens []*model.Token
	var err error
	if employeeID != "" {
		if !validCaoliaoEmployeeID(employeeID) {
			caoliaoError(c, http.StatusBadRequest, "invalid employee_id")
			return
		}
		tokens, err = model.ListCaoliaoTokensByEmployee(employeeID)
	} else {
		tokens, err = model.ListAllCaoliaoTokens()
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		caoliaoError(c, http.StatusNotFound, "employee not found")
		return
	}
	if err != nil {
		caoliaoInternalError(c, err)
		return
	}

	tokens, ok = filterCaoliaoUsageTokens(c, tokens)
	if !ok {
		return
	}
	if strings.TrimSpace(c.Query("view")) == "trend" {
		getCaoliaoTrendUsage(c, tokens, startAt, endAt)
		return
	}

	tokenIDs := make([]int, 0, len(tokens))
	userIDs := make([]int, 0, len(tokens))
	for _, token := range tokens {
		tokenIDs = append(tokenIDs, token.Id)
		userIDs = append(userIDs, token.UserId)
	}
	usageRows, err := model.QueryCaoliaoUsage(tokenIDs, startAt, endAt)
	if err != nil {
		caoliaoInternalError(c, err)
		return
	}
	employeeIDs, err := model.GetCaoliaoEmployeeIDsByUserIDs(userIDs)
	if err != nil {
		caoliaoInternalError(c, err)
		return
	}
	usageByToken := make(map[int]model.CaoliaoUsage, len(usageRows))
	for _, usage := range usageRows {
		usageByToken[usage.TokenId] = usage
	}

	items := make([]caoliaoUsageItem, 0, len(tokens))
	summary := caoliaoUsageMetrics{}
	weightedLatency := float64(0)
	for _, token := range tokens {
		usage := usageByToken[token.Id]
		requests := usage.SuccessfulRequests + usage.FailedRequests
		metrics := caoliaoUsageMetrics{
			Requests:        requests,
			InputTokens:     usage.InputTokens,
			OutputTokens:    usage.OutputTokens,
			ChargedTokens:   usage.ChargedTokens,
			Successes:       usage.SuccessfulRequests,
			Failures:        usage.FailedRequests,
			LatencyMs:       usage.AverageLatencySecond * 1000,
			RateLimitErrors: usage.RateLimitErrors,
		}
		items = append(items, caoliaoUsageItem{
			TokenId:             token.Id,
			TokenName:           token.Name,
			EmployeeId:          employeeIDs[token.UserId],
			Period:              "custom",
			Start:               startAt,
			End:                 endAt,
			caoliaoUsageMetrics: metrics,
		})
		summary.Requests += metrics.Requests
		summary.InputTokens += metrics.InputTokens
		summary.OutputTokens += metrics.OutputTokens
		summary.ChargedTokens += metrics.ChargedTokens
		summary.Successes += metrics.Successes
		summary.Failures += metrics.Failures
		summary.RateLimitErrors += metrics.RateLimitErrors
		weightedLatency += metrics.LatencyMs * float64(metrics.Requests)
	}
	if summary.Requests > 0 {
		summary.LatencyMs = weightedLatency / float64(summary.Requests)
	}
	common.ApiSuccess(c, gin.H{
		"period":  "custom",
		"start":   startAt,
		"end":     endAt,
		"summary": summary,
		"items":   items,
	})
}

func buildCaoliaoKeyMetadata(token *model.Token) caoliaoKeyMetadata {
	lastUsedAt := token.AccessedTime
	if lastUsedAt <= token.CreatedTime {
		lastUsedAt = 0
	}
	totalQuota := int64(token.RemainQuota) + int64(token.UsedQuota)
	if token.UnlimitedQuota {
		totalQuota = -1
	}
	return caoliaoKeyMetadata{
		Id:                  token.Id,
		Name:                token.Name,
		KeyMask:             "sk-" + token.GetMaskedKey(),
		Status:              caoliaoTokenStatus(token),
		TokenQuota:          totalQuota,
		UsedQuota:           token.UsedQuota,
		RequestsPerTwoHours: token.RequestsPerTwoHours,
		TokensPerTwoHours:   token.TokensPerTwoHours,
		DailyTokenQuota:     token.DailyTokenQuota,
		ExpiresAt:           token.ExpiredTime,
		CreatedAt:           token.CreatedTime,
		LastUsedAt:          lastUsedAt,
	}
}

func caoliaoTokenStatus(token *model.Token) string {
	if token.ExpiredTime != -1 && token.ExpiredTime <= common.GetTimestamp() {
		return "expired"
	}
	switch token.Status {
	case common.TokenStatusEnabled:
		if token.RemainQuota > 0 || token.UnlimitedQuota {
			return "active"
		}
		return "disabled"
	case common.TokenStatusExpired:
		return "expired"
	default:
		return "disabled"
	}
}

func validateCaoliaoKeyLimits(quota int, requestsPerTwoHours int, tokensPerTwoHours int, dailyTokenQuota int, expiresAt int64) string {
	if quota != -1 && (quota <= 0 || quota > common.MaxQuota) {
		return "token_quota is out of range"
	}
	if requestsPerTwoHours < 0 || requestsPerTwoHours > common.MaxQuota {
		return "requests_per_two_hours is out of range"
	}
	if tokensPerTwoHours < 0 || tokensPerTwoHours > common.MaxQuota {
		return "tokens_per_two_hours is out of range"
	}
	if dailyTokenQuota < 0 || dailyTokenQuota > common.MaxQuota {
		return "daily_token_quota is out of range"
	}
	if expiresAt != -1 && expiresAt <= common.GetTimestamp() {
		return "expires_at must be in the future or -1"
	}
	return ""
}

func caoliaoManagedToken(c *gin.Context) (*model.Token, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		caoliaoError(c, http.StatusBadRequest, "invalid key id")
		return nil, false
	}
	token, err := model.GetCaoliaoManagedToken(id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		caoliaoError(c, http.StatusNotFound, "key not found")
		return nil, false
	}
	if err != nil {
		caoliaoInternalError(c, err)
		return nil, false
	}
	return token, true
}

func caoliaoEmployeeID(c *gin.Context) (string, bool) {
	employeeID := strings.TrimSpace(c.Param("employee_id"))
	if !validCaoliaoEmployeeID(employeeID) {
		caoliaoError(c, http.StatusBadRequest, "invalid employee_id")
		return "", false
	}
	return employeeID, true
}

func validCaoliaoEmployeeID(employeeID string) bool {
	if employeeID == "" || len([]byte(employeeID)) > 64 {
		return false
	}
	for _, value := range employeeID {
		if value < 0x20 || value == '/' || value == '\\' {
			return false
		}
	}
	return true
}

func caoliaoUsageRange(c *gin.Context) (int64, int64, bool) {
	now := time.Now()
	endAt, err := parseCaoliaoUsageTime(strings.TrimSpace(c.Query("end")), true)
	if err != nil {
		caoliaoError(c, http.StatusBadRequest, "invalid end time")
		return 0, 0, false
	}
	if endAt == 0 || endAt > now.Unix() {
		endAt = now.Unix()
	}
	startAt, err := parseCaoliaoUsageTime(strings.TrimSpace(c.Query("start")), false)
	if err != nil {
		caoliaoError(c, http.StatusBadRequest, "invalid start time")
		return 0, 0, false
	}
	if startAt == 0 {
		startAt = time.Unix(endAt, 0).Add(-30 * 24 * time.Hour).Unix()
	}
	if startAt > endAt || time.Duration(endAt-startAt)*time.Second > maxCaoliaoUsageRange {
		caoliaoError(c, http.StatusBadRequest, "usage range must be positive and no longer than 366 days")
		return 0, 0, false
	}
	return startAt, endAt, true
}

func parseCaoliaoUsageTime(value string, endOfDay bool) (int64, error) {
	if value == "" {
		return 0, nil
	}
	if unixTime, err := strconv.ParseInt(value, 10, 64); err == nil {
		if unixTime <= 0 {
			return 0, errors.New("time must be positive")
		}
		return unixTime, nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.Unix(), nil
	}
	parsed, err := time.ParseInLocation("2006-01-02", value, caoliaoUsageLocation)
	if err != nil {
		return 0, err
	}
	if endOfDay {
		parsed = parsed.Add(24*time.Hour - time.Second)
	}
	return parsed.Unix(), nil
}

func caoliaoError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"success": false, "message": message})
}

func caoliaoInternalError(c *gin.Context, err error) {
	common.SysError("caoliao integration error: " + err.Error())
	caoliaoError(c, http.StatusInternalServerError, "internal service error")
}
