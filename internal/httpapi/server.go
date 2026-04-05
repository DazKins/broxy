package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/gorilla/securecookie"

	"github.com/personal/broxy/internal/config"
	"github.com/personal/broxy/internal/db"
	"github.com/personal/broxy/internal/domain"
	"github.com/personal/broxy/internal/pricing"
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
	sessions  *securecookie.SecureCookie
	startedAt time.Time
	version   string
}

func New(cfg *config.Config, store *db.Store, provider Provider, version string) *Server {
	hashKey := []byte(cfg.SessionSecret)
	blockKey := []byte(cfg.SessionSecret)
	return &Server{
		cfg:       cfg,
		store:     store,
		provider:  provider,
		sessions:  securecookie.New(hashKey, blockKey[:16]),
		startedAt: time.Now().UTC(),
		version:   version,
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
			r.Get("/models", s.handleAdminModels)
			r.Post("/models", s.handleCreateModel)
			r.Get("/settings", s.handleSettings)
			r.Get("/metrics", s.handlePromMetrics)
		})
	})
	r.Get("/metrics", s.handlePromMetrics)
	r.Handle("/*", ui.Handler())
	r.Handle("/", ui.Handler())
	return r
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	apiKey := clientKeyFromContext(r.Context())
	if apiKey == nil {
		writeError(w, http.StatusUnauthorized, "missing client authentication")
		return
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
		s.logRequest(r.Context(), domain.RequestRecord{
			StartedAt:      startedAt,
			FinishedAt:     finishedAt,
			APIKeyID:       apiKey.ID,
			Method:         r.Method,
			Path:           r.URL.Path,
			ModelName:      req.Model,
			BedrockModelID: bedrockModelID,
			Region:         region,
			StatusCode:     http.StatusBadGateway,
			LatencyMS:      finishedAt.Sub(startedAt).Milliseconds(),
			ErrorText:      err.Error(),
			ContentLogged:  apiKey.ContentLogging,
			RequestJSON:    s.maybeLoggedRequest(apiKey.ContentLogging, req),
			Stream:         req.Stream,
		})
		writeError(w, http.StatusBadGateway, err.Error())
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
		RequestJSON:       s.maybeLoggedRequest(apiKey.ContentLogging, req),
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
		Name           string `json:"name"`
		ContentLogging bool   `json:"content_logging"`
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
	item, err := s.store.CreateAPIKey(r.Context(), body.Name, security.KeyPrefix(token), security.HashAPIKey(token), body.ContentLogging)
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
	writeJSON(w, http.StatusCreated, map[string]any{"item": item})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"listen_addr":  s.cfg.ListenAddr,
		"db_path":      s.cfg.DBPath,
		"pricing_path": s.cfg.PricingPath,
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
			writeError(w, http.StatusUnauthorized, "missing bearer token")
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
	region := s.cfg.Upstream.Region
	if strings.TrimSpace(region) == "" {
		return nil, "", "", nil, nil, errors.New("default Bedrock region is not configured")
	}
	return nil, requested, region, reqTemp, reqMaxTokens, nil
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

func (s *Server) logRequest(ctx context.Context, record domain.RequestRecord) {
	_ = s.store.CreateRequestLog(ctx, record)
}

func (s *Server) maybeLoggedRequest(enabled bool, req ChatCompletionRequest) string {
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
