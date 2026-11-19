package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestListModelsReportsContextWindowForEligibleChannels(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	withSelfUseModeEnabled(t)
	const publicName = "deepseek-test"
	const upstreamName = "deepseek-upstream"
	const gatewayKey = "gateway-test-key"

	for _, node := range []struct {
		group         string
		payload       string
		secondPayload string
		basePath      string
	}{
		{"shared,large,unverified", `{"id":"deepseek-upstream","max_model_len":1000000}`, "", ""},
		{"shared", `{"id":"deepseek-upstream","metadata":{"context_window":"524288"}}`, "", ""},
		{"unverified", `{"id":"deepseek-upstream","max_model_len":-1}`, "", ""},
		{"rotating", `{"id":"deepseek-upstream","max_model_len":1000000}`, `{"id":"deepseek-upstream","context_length":524288}`, ""},
		{
			group:    "serving-gateway",
			payload:  `{"id":"deepseek-upstream","object":"model","created":0,"owned_by":"vllm","root":"example","parent":null,"max_model_len":524288,"permission":[]}`,
			basePath: "/serving-gateway/test-service",
		},
	} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payload := node.payload
			if r.Header.Get("Authorization") == "Bearer upstream-second-key" {
				payload = node.secondPayload
			} else if r.Header.Get("Authorization") != "Bearer upstream-test-key" {
				payload = ""
			}
			if r.Method != http.MethodGet || r.URL.Path != node.basePath+"/v1/models" || payload == "" {
				http.Error(w, "incorrect upstream model request", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"data":[%s]}`, payload)
		}))
		t.Cleanup(upstream.Close)
		channelType := constant.ChannelTypeCustom
		baseURL := upstream.URL + "/v1"
		if node.basePath != "" {
			channelType = constant.ChannelTypeDeepSeek
			baseURL = upstream.URL + node.basePath
		}
		mapping := `{"deepseek-test":"alias","alias":"deepseek-upstream"}`
		channel := model.Channel{
			Type: channelType, Status: common.ChannelStatusEnabled,
			BaseURL: &baseURL, Key: "upstream-test-key", Models: publicName,
			Group: node.group, ModelMapping: &mapping,
		}
		if node.secondPayload != "" {
			channel.ChannelInfo.IsMultiKey = true
			channel.Key += "\nupstream-second-key"
		}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, channel.AddAbilities(nil))
	}

	for _, tc := range []struct {
		group string
		want  int
	}{
		{"shared", 524288},
		{"large", 1000000},
		{"unverified", 0},
		{"rotating", 524288},
		{"serving-gateway", 524288},
	} {
		t.Run(tc.group, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			c.Request.Header.Set("Authorization", "Bearer "+gatewayKey)
			common.SetContextKey(c, constant.ContextKeyTokenGroup, tc.group)
			ListModels(c, constant.ChannelTypeOpenAI)

			require.Equal(t, http.StatusOK, recorder.Code)
			var payload struct {
				Data []struct {
					ID            string `json:"id"`
					ContextLength *int   `json:"context_length"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
			require.Len(t, payload.Data, 1)
			require.Equal(t, publicName, payload.Data[0].ID)
			if tc.want == 0 {
				require.Nil(t, payload.Data[0].ContextLength)
			} else {
				require.NotNil(t, payload.Data[0].ContextLength, "gateway did not publish the verified context window")
				require.Equal(t, tc.want, *payload.Data[0].ContextLength)
			}
		})
	}
}
