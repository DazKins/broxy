package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/gorilla/securecookie"
	"github.com/gorilla/websocket"

	"github.com/personal/broxy/internal/config"
	"github.com/personal/broxy/internal/db"
	"github.com/personal/broxy/internal/domain"
	"github.com/personal/broxy/internal/logging"
	"github.com/personal/broxy/internal/pricing"
	searchpkg "github.com/personal/broxy/internal/search"
	"github.com/personal/broxy/internal/security"
	"github.com/personal/broxy/internal/ui"
)

type Provider interface {
	Converse(ctx context.Context, req domain.ConverseRequest) (*domain.ConverseResponse, error)
}

type Server struct {
	cfg       *config.Config
	store     *db.Store
	provider  Provider
	search    searchpkg.Provider
	sessions  *securecookie.SecureCookie
	startedAt time.Time
	version   string
	logger    *slog.Logger
	respMu    sync.RWMutex
	responses map[string]storedResponse
	respItems map[string]storedResponseItem
}

var responsesUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func New(cfg *config.Config, store *db.Store, provider Provider, version string) *Server {
	return NewWithLogger(cfg, store, provider, version, logging.FromEnv())
}

func NewWithLogger(cfg *config.Config, store *db.Store, provider Provider, version string, logger *slog.Logger) *Server {
	hashKey := []byte(cfg.SessionSecret)
	blockKey := []byte(cfg.SessionSecret)
	searchProvider, err := searchpkg.NewProvider(cfg.Search)
	if err != nil && logger != nil {
		logger.Warn("search provider disabled", "error", err)
	}
	return &Server{
		cfg:       cfg,
		store:     store,
		provider:  provider,
		search:    searchProvider,
		sessions:  securecookie.New(hashKey, blockKey[:16]),
		startedAt: time.Now().UTC(),
		version:   version,
		logger:    logger,
		responses: map[string]storedResponse{},
		respItems: map[string]storedResponseItem{},
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Route("/v1", func(r chi.Router) {
		r.Use(s.requireClientAPIKey)
		r.Get("/models", s.handleListModels)
		r.Post("/chat/completions", s.handleChatCompletions)
		r.Post("/messages", s.handleAnthropicMessages)
		r.Post("/messages/count_tokens", s.handleAnthropicCountTokens)
		r.Get("/responses", s.handleResponsesWebSocket)
		r.Post("/responses", s.handleResponses)
		r.Get("/responses/{id}", s.handleGetResponse)
	})
	r.Route("/api/admin", func(r chi.Router) {
		r.Post("/auth/login", s.handleAdminLogin)
		r.Post("/auth/logout", s.handleAdminLogout)
		r.Get("/auth/me", s.requireAdmin(http.HandlerFunc(s.handleAdminMe)).ServeHTTP)
		r.Group(func(r chi.Router) {
			r.Use(s.requireAdmin)
			r.Get("/dashboard", s.handleDashboard)
			r.Get("/requests", s.handleRequests)
			r.Get("/usage", s.handleUsage)
			r.Get("/keys", s.handleKeys)
			r.Post("/keys", s.handleCreateKey)
			r.Post("/keys/{id}/revoke", s.handleRevokeKey)
			r.Get("/keys/{id}/usage", s.handleKeyUsage)
			r.Put("/keys/{id}/limit", s.handleUpdateKeyLimit)
			r.Get("/models", s.handleAdminModels)
			r.Post("/models", s.handleCreateModel)
			r.Delete("/models/{alias}", s.handleDeleteModel)
			r.Get("/settings", s.handleSettings)
			r.Get("/metrics", s.handlePromMetrics)
		})
	})
	r.Get("/metrics", s.handlePromMetrics)
	r.Handle("/*", ui.Handler())
	r.Handle("/", ui.Handler())
	return r
}

func currentMonth() string {
	return time.Now().UTC().Format("2006-01")
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	apiKey := clientKeyFromContext(r.Context())
	if apiKey == nil {
		writeError(w, http.StatusUnauthorized, "missing client authentication")
		return
	}

	// Check monthly usage limit
	if apiKey.MonthlyLimitUSD != nil && *apiKey.MonthlyLimitUSD > 0 {
		month := currentMonth()
		usage, err := s.store.GetAPIKeyMonthlyUsage(r.Context(), apiKey.ID, month)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to check usage")
			return
		}
		if usage != nil && usage.IsOverLimit {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error": map[string]any{
					"message": fmt.Sprintf("monthly usage limit exceeded for this API key. Current: $%.2f, Limit: $%.2f", usage.EstimatedCostUSD, *apiKey.MonthlyLimitUSD),
					"type":    "rate_limit_error",
				},
			})
			return
		}
	}

	startedAt := time.Now().UTC()
	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	messages, system, err := normalizeMessages(req.Messages)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	route, bedrockModelID, region, temp, maxTokens, err := s.resolveModel(r.Context(), req.Model, req.Temperature, req.MaxTokens)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	converseReq := domain.ConverseRequest{
		ModelID:     bedrockModelID,
		Region:      region,
		Messages:    messages,
		System:      system,
		Temperature: temp,
		MaxTokens:   maxTokens,
	}
	upstreamResp, err := s.provider.Converse(r.Context(), converseReq)
	finishedAt := time.Now().UTC()
	if err != nil {
		statusCode := upstreamStatusCode(err)
		s.logRequest(r.Context(), domain.RequestRecord{
			StartedAt:      startedAt,
			FinishedAt:     finishedAt,
			APIKeyID:       apiKey.ID,
			Method:         r.Method,
			Path:           r.URL.Path,
			ModelName:      req.Model,
			BedrockModelID: bedrockModelID,
			Region:         region,
			StatusCode:     statusCode,
			LatencyMS:      finishedAt.Sub(startedAt).Milliseconds(),
			ErrorText:      err.Error(),
			ContentLogged:  apiKey.ContentLogging,
			RequestJSON:    s.maybeLoggedJSON(apiKey.ContentLogging, req),
			Stream:         req.Stream,
		})
		writeError(w, statusCode, err.Error())
		return
	}
	costEntry, _ := s.store.GetPricingEntry(r.Context(), upstreamResp.ModelID, region)
	cost := pricing.EstimateCost(costEntry, upstreamResp.Usage)
	record := domain.RequestRecord{
		StartedAt:         startedAt,
		FinishedAt:        finishedAt,
		APIKeyID:          apiKey.ID,
		Method:            r.Method,
		Path:              r.URL.Path,
		ModelName:         coalesceRouteName(route, req.Model),
		BedrockModelID:    upstreamResp.ModelID,
		Region:            region,
		StatusCode:        http.StatusOK,
		LatencyMS:         upstreamResp.LatencyMS,
		InputTokens:       upstreamResp.Usage.Input,
		OutputTokens:      upstreamResp.Usage.Output,
		TotalTokens:       upstreamResp.Usage.Total,
		EstimatedCostUSD:  cost,
		ContentLogged:     apiKey.ContentLogging,
		RequestJSON:       s.maybeLoggedJSON(apiKey.ContentLogging, req),
		ResponseText:      s.maybeLogText(apiKey.ContentLogging, upstreamResp.Text),
		UpstreamRequestID: upstreamResp.RequestID,
		Stream:            req.Stream,
	}
	defer s.logRequest(r.Context(), record)

	if req.Stream {
		s.streamResponse(w, req.Model, upstreamResp)
		return
	}
	resp := ChatCompletionResponse{
		ID:      "chatcmpl_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []ChatCompletionChoice{
			{
				Index: 0,
				Message: ChoiceMessage{
					Role:    "assistant",
					Content: upstreamResp.Text,
				},
				FinishReason: normalizeFinishReason(upstreamResp.StopReason),
			},
		},
		Usage: ChatCompletionUsage{
			PromptTokens:     upstreamResp.Usage.Input,
			CompletionTokens: upstreamResp.Usage.Output,
			TotalTokens:      upstreamResp.Usage.Total,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	apiKey := clientKeyFromContext(r.Context())
	if apiKey == nil {
		writeError(w, http.StatusUnauthorized, "missing client authentication")
		return
	}
	var req ResponseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	response, status, err := s.processResponseRequest(r.Context(), apiKey, r.Method, r.URL.Path, req, time.Now().UTC())
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if req.Stream {
		s.streamResponsesResponse(w, response)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleResponsesWebSocket(w http.ResponseWriter, r *http.Request) {
	apiKey := clientKeyFromContext(r.Context())
	if apiKey == nil {
		writeError(w, http.StatusUnauthorized, "missing client authentication")
		return
	}
	conn, err := responsesUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		var envelope struct {
			Type string `json:"type"`
			ResponseRequest
		}
		if err := conn.ReadJSON(&envelope); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return
			}
			return
		}
		switch envelope.Type {
		case "", "response.create":
			envelope.Stream = true
			response, _, err := s.processResponseRequest(r.Context(), apiKey, http.MethodGet, r.URL.Path, envelope.ResponseRequest, time.Now().UTC())
			if err != nil {
				_ = conn.WriteJSON(map[string]any{
					"type": "error",
					"error": map[string]any{
						"message": err.Error(),
						"type":    "proxy_error",
					},
				})
				continue
			}
			if err := s.streamResponsesWebSocket(conn, response); err != nil {
				return
			}
		default:
			_ = conn.WriteJSON(map[string]any{
				"type": "error",
				"error": map[string]any{
					"message": fmt.Sprintf("unsupported websocket message type %q", envelope.Type),
					"type":    "proxy_error",
				},
			})
		}
	}
}

func (s *Server) processResponseRequest(ctx context.Context, apiKey *domain.APIKey, method string, path string, req ResponseRequest, startedAt time.Time) (map[string]any, int, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, http.StatusBadRequest, errors.New("model is required")
	}
	var previous *storedResponse
	if req.PreviousResponseID != "" {
		item, ok := s.lookupResponse(req.PreviousResponseID)
		if !ok {
			return nil, http.StatusBadRequest, errors.New("previous_response_id not found")
		}
		previous = &item
	}
	normalized, err := normalizeResponseRequestWithResolver(req, previous, s.lookupResponseItem)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	route, bedrockModelID, region, temp, maxTokens, err := s.resolveModel(ctx, req.Model, req.Temperature, req.MaxOutputTokens)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	converseReq := domain.ConverseRequest{
		ModelID:     bedrockModelID,
		Region:      region,
		Messages:    normalized.Messages,
		System:      normalized.System,
		Temperature: temp,
		MaxTokens:   maxTokens,
		Tools:       normalized.Tools,
		ToolChoice:  normalized.ToolChoice,
	}
	if normalized.ToolChoice != nil && normalized.ToolChoice.Type == "none" {
		converseReq.Tools = nil
		converseReq.ToolChoice = nil
	}
	upstreamResp, err := s.provider.Converse(ctx, converseReq)
	finishedAt := time.Now().UTC()
	if err != nil {
		statusCode := upstreamStatusCode(err)
		s.logRequest(ctx, domain.RequestRecord{
			StartedAt:      startedAt,
			FinishedAt:     finishedAt,
			APIKeyID:       apiKey.ID,
			Method:         method,
			Path:           path,
			ModelName:      req.Model,
			BedrockModelID: bedrockModelID,
			Region:         region,
			StatusCode:     statusCode,
			LatencyMS:      finishedAt.Sub(startedAt).Milliseconds(),
			ErrorText:      err.Error(),
			ContentLogged:  apiKey.ContentLogging,
			RequestJSON:    s.maybeLoggedJSON(apiKey.ContentLogging, req),
			Stream:         req.Stream,
		})
		return nil, statusCode, err
	}
	costEntry, _ := s.store.GetPricingEntry(ctx, upstreamResp.ModelID, region)
	cost := pricing.EstimateCost(costEntry, upstreamResp.Usage)
	record := domain.RequestRecord{
		StartedAt:         startedAt,
		FinishedAt:        finishedAt,
		APIKeyID:          apiKey.ID,
		Method:            method,
		Path:              path,
		ModelName:         coalesceRouteName(route, req.Model),
		BedrockModelID:    upstreamResp.ModelID,
		Region:            region,
		StatusCode:        http.StatusOK,
		LatencyMS:         upstreamResp.LatencyMS,
		InputTokens:       upstreamResp.Usage.Input,
		OutputTokens:      upstreamResp.Usage.Output,
		TotalTokens:       upstreamResp.Usage.Total,
		EstimatedCostUSD:  cost,
		ContentLogged:     apiKey.ContentLogging,
		RequestJSON:       s.maybeLoggedJSON(apiKey.ContentLogging, req),
		ResponseText:      s.maybeLogText(apiKey.ContentLogging, upstreamResp.Text),
		UpstreamRequestID: upstreamResp.RequestID,
		Stream:            req.Stream,
	}
	defer s.logRequest(ctx, record)

	responseID := "resp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	response := buildResponseEnvelope(responseID, req, normalized, upstreamResp)
	s.logResponseDebug(path, responseID, req, upstreamResp, response)
	itemIDs, responseItems := responseOutputItems(response)
	message := upstreamResp.Message
	if len(message.Blocks) == 0 && strings.TrimSpace(message.Content) == "" && strings.TrimSpace(upstreamResp.Text) != "" {
		message = domain.BedrockChatMessage{
			Role:    "assistant",
			Content: upstreamResp.Text,
			Blocks: []domain.BedrockContentBlock{{
				Type: "text",
				Text: upstreamResp.Text,
			}},
		}
	}
	s.storeResponse(responseID, storedResponse{
		Response: response,
		System:   cloneStrings(normalized.System),
		Messages: append(cloneMessages(normalized.Messages), message),
		ItemIDs:  itemIDs,
	})
	s.storeResponseItems(responseItems)
	return response, http.StatusOK, nil
}

func upstreamStatusCode(err error) int {
	type httpStatusCoder interface {
		HTTPStatusCode() int
	}
	var statusErr httpStatusCoder
	if errors.As(err, &statusErr) {
		if statusCode := statusErr.HTTPStatusCode(); statusCode > 0 {
			return statusCode
		}
	}
	return http.StatusBadGateway
}

func (s *Server) handleGetResponse(w http.ResponseWriter, r *http.Request) {
	response, ok := s.lookupResponse(chi.URLParam(r, "id"))
	if !ok {
		writeError(w, http.StatusNotFound, "response not found")
		return
	}
	writeJSON(w, http.StatusOK, response.Response)
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListModelRoutes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if !item.Enabled {
			continue
		}
		data = append(data, map[string]any{
			"id":       item.Alias,
			"object":   "model",
			"created":  item.CreatedAt.Unix(),
			"owned_by": "broxy",
			"metadata": map[string]any{
				"bedrock_model_id": item.BedrockModelID,
				"region":           item.Region,
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	user, err := s.store.GetAdminUser(r.Context(), body.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if user == nil || security.CheckPassword(user.PasswordHash, body.Password) != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	value := map[string]string{
		"username": user.Username,
		"exp":      time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano),
	}
	encoded, err := s.sessions.Encode("broxy_session", value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "broxy_session",
		Value:    encoded,
		Path:     "/",
		HttpOnly: true,
		Secure:   false,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": user.Username})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "broxy_session",
		Value:    "",
		MaxAge:   -1,
		Path:     "/",
		HttpOnly: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"username": adminUsernameFromContext(r.Context()),
	})
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	metrics, err := s.store.DashboardMetrics(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"summary": domain.AppSummary{
			StartedAt: s.startedAt,
			Version:   s.version,
		},
		"metrics": metrics,
	})
}

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.store.ListRequestLogs(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.UsageBreakdown(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListAPIKeys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name            string   `json:"name"`
		ContentLogging  bool     `json:"content_logging"`
		MonthlyLimitUSD *float64 `json:"monthly_limit_usd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	token, err := security.RandomToken("bpx_", 24)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	item, err := s.store.CreateAPIKey(r.Context(), body.Name, security.KeyPrefix(token), security.HashAPIKey(token), body.ContentLogging, body.MonthlyLimitUSD)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"item": item,
		"key":  token,
	})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DisableAPIKey(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleKeyUsage(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "id")
	month := r.URL.Query().Get("month")
	if month == "" {
		month = currentMonth()
	}

	usage, err := s.store.GetAPIKeyMonthlyUsage(r.Context(), keyID, month)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if usage == nil {
		usage = &domain.APIKeyUsageSummary{
			APIKeyID: keyID,
			Month:    month,
		}
	}
	writeJSON(w, http.StatusOK, usage)
}

func (s *Server) handleUpdateKeyLimit(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "id")
	var body struct {
		MonthlyLimitUSD *float64 `json:"monthly_limit_usd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.store.UpdateAPIKeyLimit(r.Context(), keyID, body.MonthlyLimitUSD); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListModelRoutes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleCreateModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Alias       string   `json:"alias"`
		ModelID     string   `json:"model_id"`
		Region      string   `json:"region"`
		Temperature *float64 `json:"temperature"`
		MaxTokens   *int     `json:"max_tokens"`
		Enabled     *bool    `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	previous, err := s.store.GetModelRoute(r.Context(), body.Alias)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	item, err := s.store.UpsertModelRoute(r.Context(), domain.ModelRoute{
		Alias:              body.Alias,
		BedrockModelID:     body.ModelID,
		Region:             body.Region,
		Enabled:            enabled,
		DefaultTemperature: body.Temperature,
		DefaultMaxTokens:   body.MaxTokens,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.syncModelPricing(r.Context(), previous, item); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"item": item})
}

func (s *Server) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	alias := chi.URLParam(r, "alias")
	previous, err := s.store.DeleteModelRoute(r.Context(), alias)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if previous == nil {
		writeError(w, http.StatusNotFound, "model route not found")
		return
	}
	if err := s.syncModelPricing(r.Context(), previous, nil); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) syncModelPricing(ctx context.Context, previous, current *domain.ModelRoute) error {
	if current != nil {
		entry, err := pricing.EnsureEntry(s.cfg.PricingPath, current.BedrockModelID, current.Region)
		if err != nil {
			return err
		}
		if err := s.store.UpsertPricingEntries(ctx, []domain.PricingEntry{*entry}); err != nil {
			return err
		}
	}
	if previous == nil {
		return nil
	}
	if current != nil && previous.BedrockModelID == current.BedrockModelID && previous.Region == current.Region {
		return nil
	}
	return s.removeUnusedPricingEntry(ctx, previous.BedrockModelID, previous.Region)
}

func (s *Server) removeUnusedPricingEntry(ctx context.Context, modelID, region string) error {
	routes, err := s.store.ListModelRoutes(ctx)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if route.BedrockModelID == modelID && route.Region == region {
			return nil
		}
	}
	if _, err := pricing.RemoveEntry(s.cfg.PricingPath, modelID, region); err != nil {
		return err
	}
	return s.store.DeletePricingEntry(ctx, modelID, region)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"listen_addr":  s.cfg.ListenAddr,
		"config_dir":   s.cfg.ConfigDir,
		"state_dir":    s.cfg.StateDir,
		"db_path":      s.cfg.DBPath,
		"pricing_path": s.cfg.PricingPath,
		"log_dir":      s.cfg.LogDir(),
		"upstream": map[string]any{
			"mode":    s.cfg.Upstream.Mode,
			"region":  s.cfg.Upstream.Region,
			"profile": s.cfg.Upstream.Profile,
		},
	})
}

func (s *Server) handlePromMetrics(w http.ResponseWriter, r *http.Request) {
	metrics, err := s.store.DashboardMetrics(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "broxy_requests_total %d\n", metrics.TotalRequests)
	fmt.Fprintf(w, "broxy_requests_success_total %d\n", metrics.SuccessRequests)
	fmt.Fprintf(w, "broxy_requests_error_total %d\n", metrics.ErrorRequests)
	fmt.Fprintf(w, "broxy_input_tokens_total %d\n", metrics.TotalInputTokens)
	fmt.Fprintf(w, "broxy_output_tokens_total %d\n", metrics.TotalOutputTokens)
	fmt.Fprintf(w, "broxy_estimated_cost_usd %f\n", metrics.TotalCostUSD)
	fmt.Fprintf(w, "broxy_latency_avg_ms %f\n", metrics.AverageLatencyMS)
}

func (s *Server) requireClientAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := security.NormalizeBearer(r.Header.Get("Authorization"))
		if token == "" {
			token = strings.TrimSpace(r.Header.Get("X-Api-Key"))
		}
		if token == "" {
			writeError(w, http.StatusUnauthorized, "missing client API key")
			return
		}
		item, err := s.store.AuthenticateAPIKey(r.Context(), security.HashAPIKey(token))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if item == nil || !item.Enabled {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKeyContextKey{}, item)))
	})
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("broxy_session")
		if err != nil {
			writeError(w, http.StatusUnauthorized, "missing admin session")
			return
		}
		value := map[string]string{}
		if err := s.sessions.Decode("broxy_session", cookie.Value, &value); err != nil {
			writeError(w, http.StatusUnauthorized, "invalid admin session")
			return
		}
		exp, err := time.Parse(time.RFC3339Nano, value["exp"])
		if err != nil || time.Now().After(exp) {
			writeError(w, http.StatusUnauthorized, "expired admin session")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adminContextKey{}, value["username"])))
	})
}

func (s *Server) resolveModel(ctx context.Context, requested string, reqTemp *float64, reqMaxTokens *int) (*domain.ModelRoute, string, string, *float64, *int, error) {
	if route, err := s.store.GetModelRoute(ctx, requested); err != nil {
		return nil, "", "", nil, nil, err
	} else if route != nil {
		if !route.Enabled {
			return nil, "", "", nil, nil, fmt.Errorf("model %q is currently disabled", requested)
		}
		temp := reqTemp
		if temp == nil {
			temp = route.DefaultTemperature
		}
		maxTokens := reqMaxTokens
		if maxTokens == nil {
			maxTokens = route.DefaultMaxTokens
		}
		return route, route.BedrockModelID, route.Region, temp, maxTokens, nil
	}
	return nil, "", "", nil, nil, fmt.Errorf("model %q is not available; configure it as a model alias first", requested)
}

func (s *Server) streamResponse(w http.ResponseWriter, model string, upstreamResp *domain.ConverseResponse) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	streamID := "chatcmpl_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	writeSSE(w, map[string]any{
		"id":      streamID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{"role": "assistant"},
		}},
	})
	flusher.Flush()
	for _, chunk := range chunkText(upstreamResp.Text, 48) {
		writeSSE(w, map[string]any{
			"id":      streamID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"content": chunk},
			}},
		})
		flusher.Flush()
	}
	writeSSE(w, map[string]any{
		"id":      streamID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": normalizeFinishReason(upstreamResp.StopReason),
		}},
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) streamResponsesResponse(w http.ResponseWriter, response map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	if err := s.streamResponsesEvents(response, func(payload map[string]any) error {
		writeSSE(w, payload)
		flusher.Flush()
		return nil
	}); err != nil {
		return
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) streamResponsesWebSocket(conn *websocket.Conn, response map[string]any) error {
	return s.streamResponsesEvents(response, func(payload map[string]any) error {
		return conn.WriteJSON(payload)
	})
}

func (s *Server) streamResponsesEvents(response map[string]any, send func(map[string]any) error) error {
	output, _ := response["output"].([]map[string]any)
	responseID, _ := response["id"].(string)
	sequence := 0
	writeEvent := func(payload map[string]any) error {
		payload["sequence_number"] = sequence
		payload["response_id"] = responseID
		sequence++
		return send(payload)
	}

	if err := writeEvent(map[string]any{
		"type":     "response.created",
		"response": cloneResponseForEvent(response, "in_progress", []map[string]any{}),
	}); err != nil {
		return err
	}
	if err := writeEvent(map[string]any{
		"type":     "response.in_progress",
		"response": cloneResponseForEvent(response, "in_progress", []map[string]any{}),
	}); err != nil {
		return err
	}
	for outputIndex, item := range output {
		itemType, _ := item["type"].(string)
		itemID, _ := item["id"].(string)
		switch itemType {
		case "message":
			if err := writeEvent(map[string]any{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]any{
					"id":      itemID,
					"type":    "message",
					"status":  "in_progress",
					"role":    "assistant",
					"content": []map[string]any{},
				},
			}); err != nil {
				return err
			}
			content, _ := item["content"].([]map[string]any)
			for contentIndex, part := range content {
				text, _ := part["text"].(string)
				if err := writeEvent(map[string]any{
					"type":          "response.content_part.added",
					"item_id":       itemID,
					"output_index":  outputIndex,
					"content_index": contentIndex,
					"part": map[string]any{
						"type":        "output_text",
						"text":        "",
						"annotations": []any{},
					},
				}); err != nil {
					return err
				}
				for _, chunk := range chunkText(text, 48) {
					if err := writeEvent(map[string]any{
						"type":          "response.output_text.delta",
						"item_id":       itemID,
						"output_index":  outputIndex,
						"content_index": contentIndex,
						"delta":         chunk,
					}); err != nil {
						return err
					}
				}
				if err := writeEvent(map[string]any{
					"type":          "response.output_text.done",
					"item_id":       itemID,
					"output_index":  outputIndex,
					"content_index": contentIndex,
					"text":          text,
				}); err != nil {
					return err
				}
				if err := writeEvent(map[string]any{
					"type":          "response.content_part.done",
					"item_id":       itemID,
					"output_index":  outputIndex,
					"content_index": contentIndex,
					"part":          part,
				}); err != nil {
					return err
				}
			}
			if err := writeEvent(map[string]any{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item":         item,
			}); err != nil {
				return err
			}
		case "function_call":
			if err := writeEvent(map[string]any{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]any{
					"id":        itemID,
					"type":      "function_call",
					"status":    "in_progress",
					"call_id":   item["call_id"],
					"name":      item["name"],
					"arguments": "",
				},
			}); err != nil {
				return err
			}
			arguments, _ := item["arguments"].(string)
			for _, chunk := range chunkText(arguments, 96) {
				if err := writeEvent(map[string]any{
					"type":         "response.function_call_arguments.delta",
					"item_id":      itemID,
					"output_index": outputIndex,
					"delta":        chunk,
				}); err != nil {
					return err
				}
			}
			if err := writeEvent(map[string]any{
				"type":         "response.function_call_arguments.done",
				"item_id":      itemID,
				"output_index": outputIndex,
				"arguments":    arguments,
				"name":         item["name"],
				"call_id":      item["call_id"],
			}); err != nil {
				return err
			}
			if err := writeEvent(map[string]any{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item":         item,
			}); err != nil {
				return err
			}
		}
	}
	return writeEvent(map[string]any{
		"type":     "response.completed",
		"response": response,
	})
}

func cloneResponseForEvent(response map[string]any, status string, output []map[string]any) map[string]any {
	cloned := make(map[string]any, len(response))
	for k, v := range response {
		cloned[k] = v
	}
	cloned["status"] = status
	cloned["output"] = output
	return cloned
}

func (s *Server) logRequest(ctx context.Context, record domain.RequestRecord) {
	_ = s.store.CreateRequestLog(ctx, record)
}

func (s *Server) maybeLoggedJSON(enabled bool, req any) string {
	if !enabled {
		return ""
	}
	body, _ := json.Marshal(req)
	return string(body)
}

func (s *Server) maybeLogText(enabled bool, value string) string {
	if !enabled {
		return ""
	}
	return value
}

func (s *Server) logResponseDebug(path string, responseID string, req ResponseRequest, upstreamResp *domain.ConverseResponse, response map[string]any) {
	if s.logger == nil {
		return
	}
	output, _ := response["output"].([]map[string]any)
	hasToolCall := false
	emptyToolArguments := false
	for _, item := range output {
		if itemType, _ := item["type"].(string); itemType == "function_call" {
			hasToolCall = true
			if arguments, _ := item["arguments"].(string); strings.TrimSpace(arguments) == "{}" {
				emptyToolArguments = true
			}
		}
	}
	s.logger.Debug("responses upstream response",
		"path", path,
		"response_id", responseID,
		"model", req.Model,
		"stream", req.Stream,
		"stop_reason", upstreamResp.StopReason,
		"input_tokens", upstreamResp.Usage.Input,
		"output_tokens", upstreamResp.Usage.Output,
		"total_tokens", upstreamResp.Usage.Total,
		"text_bytes", len(upstreamResp.Text),
		"message_blocks", len(upstreamResp.Message.Blocks),
		"output_items", len(output),
		"has_tool_call", hasToolCall,
		"empty_tool_arguments", emptyToolArguments,
		"raw_response", upstreamResp.RawResponse,
	)
}

func (s *Server) storeResponse(id string, item storedResponse) {
	s.respMu.Lock()
	defer s.respMu.Unlock()
	s.responses[id] = item
}

func (s *Server) storeResponseItems(items map[string]storedResponseItem) {
	s.respMu.Lock()
	defer s.respMu.Unlock()
	for itemID, item := range items {
		s.respItems[itemID] = storedResponseItem{
			Messages: cloneMessages(item.Messages),
		}
	}
}

func (s *Server) lookupResponse(id string) (storedResponse, bool) {
	s.respMu.RLock()
	defer s.respMu.RUnlock()
	item, ok := s.responses[id]
	return item, ok
}

func (s *Server) lookupResponseItem(id string) (storedResponseItem, bool) {
	s.respMu.RLock()
	defer s.respMu.RUnlock()
	item, ok := s.respItems[id]
	if ok {
		return storedResponseItem{
			Messages: cloneMessages(item.Messages),
		}, true
	}
	for _, response := range s.responses {
		output, _ := response.Response["output"].([]map[string]any)
		for _, outputItem := range output {
			itemID, _ := outputItem["id"].(string)
			if itemID != id {
				continue
			}
			messages := outputItemMessages(outputItem)
			if len(messages) == 0 {
				return storedResponseItem{}, false
			}
			return storedResponseItem{
				Messages: messages,
			}, true
		}
	}
	return storedResponseItem{}, false
}

type clientKeyContextKey struct{}
type adminContextKey struct{}

func clientKeyFromContext(ctx context.Context) *domain.APIKey {
	v, _ := ctx.Value(clientKeyContextKey{}).(*domain.APIKey)
	return v
}

func adminUsernameFromContext(ctx context.Context) string {
	v, _ := ctx.Value(adminContextKey{}).(string)
	return v
}

func normalizeFinishReason(value string) string {
	switch value {
	case "", "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

func chunkText(text string, chunkSize int) []string {
	if chunkSize <= 0 {
		chunkSize = 48
	}
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	items := make([]string, 0, (len(runes)/chunkSize)+1)
	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		items = append(items, string(runes[i:end]))
	}
	return items
}

func coalesceRouteName(route *domain.ModelRoute, requested string) string {
	if route != nil {
		return route.Alias
	}
	return requested
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "proxy_error",
		},
	})
}

func writeSSE(w http.ResponseWriter, value any) {
	body, _ := json.Marshal(value)
	fmt.Fprintf(w, "data: %s\n\n", body)
}
