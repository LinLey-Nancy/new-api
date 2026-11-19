package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/samber/hot"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

const upstreamContextTimeout = 3 * time.Second

var (
	upstreamContextCache   = hot.NewHotCache[string, map[string]int](hot.LRU, 512).Build()
	upstreamContextFlights singleflight.Group
	upstreamContextSlots   = make(chan struct{}, 8)
)

// publishModelContextWindows advertises a window only when every eligible
// upstream confirms one. Lower-priority channels can still receive retries.
func publishModelContextWindows(c *gin.Context, groups []string, cards []dto.OpenAIModels) {
	if len(cards) == 0 || len(groups) == 0 || c.Request == nil {
		return
	}
	names := make([]string, 0, len(cards))
	for _, card := range cards {
		names = append(names, card.Id)
	}

	var abilities []model.Ability
	routes := model.DB.Select("model", "channel_id").Distinct().
		Where(map[string]any{"enabled": true, "group": groups, "model": names})
	if channelID, ok := common.GetContextKey(c, constant.ContextKeyTokenSpecificChannelId); ok {
		id, err := strconv.Atoi(fmt.Sprint(channelID))
		if err != nil {
			return
		}
		routes = routes.Where("channel_id = ?", id)
	}
	if err := routes.Find(&abilities).Error; err != nil || len(abilities) == 0 {
		return
	}
	channelIDs := make([]int, 0, len(abilities))
	seenChannels := make(map[int]bool)
	for _, ability := range abilities {
		if !seenChannels[ability.ChannelId] {
			channelIDs = append(channelIDs, ability.ChannelId)
			seenChannels[ability.ChannelId] = true
		}
	}
	var channels []model.Channel
	if err := model.DB.Where("id IN ?", channelIDs).Find(&channels).Error; err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), upstreamContextTimeout)
	defer cancel()
	confirmed := make(map[int]map[string]int, len(channels))
	mappings := make(map[int]*gin.Context, len(channels))
	var mu sync.Mutex
	var lookups errgroup.Group
	lookups.SetLimit(cap(upstreamContextSlots))
	for i := range channels {
		channel := &channels[i]
		mapping := &gin.Context{}
		mapping.Set("model_mapping", channel.GetModelMapping())
		mappings[channel.Id] = mapping
		if channel.Status != common.ChannelStatusEnabled || !canDiscoverChannelContext(channel) {
			continue
		}
		lookups.Go(func() error {
			windows := getChannelContextWindows(ctx, channel)
			mu.Lock()
			confirmed[channel.Id] = windows
			mu.Unlock()
			return nil
		})
	}
	_ = lookups.Wait()

	minimums := make(map[string]int)
	unconfirmed := make(map[string]bool)
	for _, ability := range abilities {
		name := ability.Model
		windows := confirmed[ability.ChannelId]
		if len(windows) == 0 {
			unconfirmed[name] = true
			continue
		}
		info := &relaycommon.RelayInfo{
			OriginModelName: name,
			ChannelMeta:     &relaycommon.ChannelMeta{UpstreamModelName: name},
		}
		if err := helper.ModelMappedHelper(mappings[ability.ChannelId], info, nil); err != nil {
			unconfirmed[name] = true
			continue
		}
		window := windows[info.UpstreamModelName]
		if window == 0 {
			unconfirmed[name] = true
		} else if previous := minimums[name]; previous == 0 || window < previous {
			minimums[name] = window
		}
	}
	for i := range cards {
		if !unconfirmed[cards[i].Id] {
			cards[i].ContextLength = minimums[cards[i].Id]
		}
	}
}

func canDiscoverChannelContext(channel *model.Channel) bool {
	switch channel.Type {
	case constant.ChannelTypeCustom, constant.ChannelTypeOpenAI, constant.ChannelTypeDeepSeek, constant.ChannelTypeAdvancedCustom:
		return channel.BaseURL != nil && strings.TrimSpace(*channel.BaseURL) != ""
	default:
		return false
	}
}

func getChannelContextWindows(ctx context.Context, channel *model.Channel) map[string]int {
	// Inspect all usable credentials without advancing the relay's key rotation.
	keys := []string{strings.TrimSpace(channel.Key)}
	if channel.ChannelInfo.IsMultiKey {
		keys = nil
		for index, candidate := range channel.GetKeys() {
			status, present := channel.ChannelInfo.MultiKeyStatusList[index]
			if key := strings.TrimSpace(candidate); key != "" && (!present || status == common.ChannelStatusEnabled) {
				keys = append(keys, key)
			}
		}
		if len(keys) == 0 {
			return nil
		}
	}
	cacheIdentity := fmt.Sprintf("%d:%d:%q:%q:%q:%v:%q",
		channel.Id, channel.Type, channel.GetBaseURL(), keys, channel.GetSetting().Proxy,
		channel.GetHeaderOverride(), channel.OtherSettings)
	cacheKey := fmt.Sprintf("%x", sha256.Sum256([]byte(cacheIdentity)))
	if windows, found, _ := upstreamContextCache.Get(cacheKey); found {
		return windows
	}
	if ctx.Err() != nil {
		return nil
	}

	result := upstreamContextFlights.DoChan(cacheKey, func() (any, error) {
		if windows, found, _ := upstreamContextCache.Get(cacheKey); found {
			return windows, nil
		}
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upstreamContextTimeout)
		defer cancel()
		select {
		case upstreamContextSlots <- struct{}{}:
			defer func() { <-upstreamContextSlots }()
		case <-probeCtx.Done():
			return nil, probeCtx.Err()
		}
		var windows map[string]int
		for i, key := range keys {
			advertised, err := fetchChannelContextWindows(probeCtx, channel, key)
			if err != nil {
				upstreamContextCache.SetWithTTL(cacheKey, nil, 10*time.Second)
				return nil, err
			}
			if i == 0 {
				windows = advertised
			} else {
				for modelID, previous := range windows {
					switch reported := advertised[modelID]; {
					case reported == 0:
						delete(windows, modelID)
					case reported < previous:
						windows[modelID] = reported
					}
				}
			}
			if len(windows) == 0 {
				break
			}
		}
		upstreamContextCache.SetWithTTL(cacheKey, windows, time.Minute)
		return windows, nil
	})
	select {
	case response := <-result:
		windows, _ := response.Val.(map[string]int)
		return windows
	case <-ctx.Done():
		return nil
	}
}

func fetchChannelContextWindows(ctx context.Context, channel *model.Channel, key string) (map[string]int, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(channel.GetBaseURL()), "/")
	var url string
	var headers http.Header
	var err error
	if channel.Type == constant.ChannelTypeAdvancedCustom {
		url, headers, err = buildAdvancedCustomModelsRequest(channel, baseURL, key)
	} else {
		url = strings.TrimSuffix(baseURL, "/v1") + "/v1/models"
		headers, err = buildFetchModelsHeaders(channel, key)
	}
	if err != nil {
		return nil, err
	}
	body, err := getFetchModelsResponseBody(ctx, http.MethodGet, url, channel, headers, 8<<20)
	if err != nil {
		return nil, err
	}

	var cards struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := common.Unmarshal(body, &cards); err != nil {
		return nil, err
	}

	windows := make(map[string]int)
	for _, card := range cards.Data {
		var id string
		if err := common.Unmarshal(card["id"], &id); err != nil || id == "" {
			continue
		}
		var metadata map[string]json.RawMessage
		_ = common.Unmarshal(card["metadata"], &metadata)
		window := 0
		for _, attributes := range []map[string]json.RawMessage{card, metadata} {
			for _, field := range []string{"context_length", "max_model_len", "context_window", "max_context_length", "max_sequence_length", "max_position_embeddings"} {
				raw := attributes[field]
				if len(raw) == 0 {
					continue
				}
				value := string(raw)
				if raw[0] == '"' && common.Unmarshal(raw, &value) != nil {
					continue
				}
				count, err := strconv.Atoi(value)
				if err == nil && count > 0 && (window == 0 || count < window) {
					window = count
				}
			}
		}
		if previous, exists := windows[id]; !exists || window < previous {
			windows[id] = window
		}
	}
	return windows, nil
}
