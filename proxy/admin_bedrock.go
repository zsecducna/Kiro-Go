package proxy

// Admin endpoint for registering a native Amazon Bedrock account (a static IAM
// access key + region that calls Bedrock Runtime directly). Mirrors the shape of
// handleAdminAddCustomApiAccount so the bot and panel treat it the same way.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"kiro-go/config"
	"kiro-go/logger"

	"github.com/google/uuid"
)

// adminAddBedrockRequest is the body for POST /admin/add_bedrock_account.
type adminAddBedrockRequest struct {
	Nickname        string            `json:"nickname,omitempty"`     // Display name (defaults to "Bedrock <region>")
	Region          string            `json:"region"`                 // AWS region, e.g. us-east-1 (required)
	AccessKeyID     string            `json:"accessKeyId"`            // IAM access key id (required)
	SecretAccessKey string            `json:"secretAccessKey"`        // IAM secret (required)
	SessionToken    string            `json:"sessionToken,omitempty"` // Only for STS/temporary credentials
	ModelMap        map[string]string `json:"modelMap,omitempty"`     // Optional client-model -> Bedrock-model-id overrides
	UseConverse     bool              `json:"useConverse,omitempty"`  // Use the Converse API (required for non-Anthropic models)
	ProxyURL        string            `json:"proxyUrl,omitempty"`     // Optional per-account outbound proxy (user:pass@host:port)
	Weight          int               `json:"weight,omitempty"`       // Load-balancing weight (0/1 normal)
	Enabled         *bool             `json:"enabled,omitempty"`      // Route traffic immediately (default true)
}

// handleAdminAddBedrockAccount POST /admin/add_bedrock_account.
func (h *Handler) handleAdminAddBedrockAccount(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	var req adminAddBedrockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	id, status, err := h.addBedrockAccount(req)
	if err != nil {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": id})
}

// addBedrockAccount validates the request and persists a new bedrock account.
// Returns the new account id, an HTTP status, and an error (nil on success).
func (h *Handler) addBedrockAccount(req adminAddBedrockRequest) (string, int, error) {
	region := strings.TrimSpace(req.Region)
	ak := strings.TrimSpace(req.AccessKeyID)
	sk := strings.TrimSpace(req.SecretAccessKey)
	if region == "" {
		return "", http.StatusBadRequest, fmt.Errorf("region is required")
	}
	if ak == "" || sk == "" {
		return "", http.StatusBadRequest, fmt.Errorf("accessKeyId and secretAccessKey are required")
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	nickname := strings.TrimSpace(req.Nickname)
	if nickname == "" {
		nickname = "Bedrock " + region
	}

	account := config.Account{
		ID:                     uuid.New().String(),
		Nickname:               nickname,
		AuthMethod:             "bedrock",
		Provider:               "Bedrock",
		Region:                 region,
		BedrockAccessKeyID:     ak,
		BedrockSecretAccessKey: sk,
		BedrockSessionToken:    strings.TrimSpace(req.SessionToken),
		BedrockModelMap:        req.ModelMap,
		BedrockUseConverse:     req.UseConverse,
		ProxyURL:               strings.TrimSpace(req.ProxyURL),
		Tags:                   []string{"Bedrock"},
		Weight:                 req.Weight,
		Enabled:                enabled,
		SubscriptionType:       "Bedrock",
	}

	if err := config.AddAccount(account); err != nil {
		return "", http.StatusInternalServerError, err
	}
	logger.Infof("[Bedrock] added account %s (region %s)", account.ID, region)
	return account.ID, http.StatusOK, nil
}
