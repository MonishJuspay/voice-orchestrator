package handlers

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"log"
	"net/http"

	"go.uber.org/zap"

	"orchestration-api-go/internal/allocator"
	"orchestration-api-go/internal/models"
)

// AllocateHandler handles pod allocation requests
type AllocateHandler struct {
	allocator allocator.Interface
	logger    *zap.Logger
}

// NewAllocateHandler creates a new allocation handler
func NewAllocateHandler(allocator allocator.Interface, logger *zap.Logger) *AllocateHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AllocateHandler{
		allocator: allocator,
		logger:    logger,
	}
}

// Handle handles POST /api/v1/allocate
func (h *AllocateHandler) Handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Decode JSON body
	var req models.AllocationRequest
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.logger.Warn("failed to decode allocation request", zap.Error(err))
		respondWithError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate required fields
	if req.CallSID == "" {
		respondWithError(w, http.StatusBadRequest, "call_sid is required")
		return
	}

	// Default provider and flow
	provider := req.Provider
	if provider == "" {
		provider = "twilio"
	}
	flow := req.Flow
	if flow == "" {
		flow = "v2"
	}

	// Call allocator
	result, err := h.allocator.Allocate(ctx, req.CallSID, req.MerchantID, provider, flow, req.Template)
	if err != nil {
		h.logger.Error("allocation failed",
			zap.Error(err),
			zap.String("call_sid", req.CallSID),
			zap.String("merchant_id", req.MerchantID),
		)

		switch err {
		case allocator.ErrNoPodsAvailable:
			respondWithError(w, http.StatusServiceUnavailable, "no pods available")
		case allocator.ErrInvalidCallSID:
			respondWithError(w, http.StatusBadRequest, "invalid call_sid")
		case allocator.ErrDrainingPod:
			respondWithError(w, http.StatusServiceUnavailable, "pod is draining")
		default:
			respondWithError(w, http.StatusInternalServerError, "allocation failed")
		}
		return
	}

	// Return JSON response
	response := map[string]interface{}{
		"success":      true,
		"pod_name":     result.PodName,
		"ws_url":       result.WSURL,
		"source_pool":  result.SourcePool,
		"was_existing": result.WasExisting,
	}

	respondWithJSON(w, http.StatusOK, response)
}

// TwiMLResponse represents a TwiML XML response
type TwiMLResponse struct {
	XMLName xml.Name `xml:"Response"`
	Connect struct {
		Stream struct {
			URL string `xml:"url,attr"`
		} `xml:"Stream"`
	} `xml:"Connect"`
}

// HandleTwilio handles POST /api/v1/twilio/allocate
func (h *AllocateHandler) HandleTwilio(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse form data
	if err := r.ParseForm(); err != nil {
		h.logger.Warn("failed to parse Twilio form data", zap.Error(err))
		respondWithXMLError(w, "invalid form data")
		return
	}

	// Get CallSid from form data
	callSID := r.FormValue("CallSid")
	if callSID == "" {
		respondWithXMLError(w, "missing CallSid")
		return
	}

	// Get merchant_id, flow, and template from query parameters
	merchantID := r.URL.Query().Get("merchant_id")
	flow := r.URL.Query().Get("flow")
	if flow == "" {
		flow = "v2"
	}
	template := r.URL.Query().Get("template")

	h.logger.Info("twilio allocation request",
		zap.String("call_sid", callSID),
		zap.String("merchant_id", merchantID),
		zap.String("flow", flow),
		zap.String("template", template),
	)

	// Call allocator
	result, err := h.allocator.Allocate(ctx, callSID, merchantID, "twilio", flow, template)
	if err != nil {
		h.logger.Error("twilio allocation failed",
			zap.Error(err),
			zap.String("call_sid", callSID),
		)
		respondWithXMLError(w, "allocation failed")
		return
	}

	// Build TwiML response
	twiml := TwiMLResponse{}
	twiml.Connect.Stream.URL = result.WSURL

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	if err := xml.NewEncoder(w).Encode(twiml); err != nil {
		h.logger.Error("failed to encode TwiML response", zap.Error(err))
	}
}

// PlivoXMLResponse represents a Plivo XML response
// PlivoXMLResponse represents a Plivo XML response
type PlivoXMLResponse struct {
	XMLName xml.Name `xml:"Response"`
	Stream  struct {
		URL             string `xml:",chardata"`
		Bidirectional   bool   `xml:"bidirectional,attr"`
		KeepCallAlive   bool   `xml:"keepCallAlive,attr"`
		ContentType     string `xml:"contentType,attr"`
		NoiseCancellation bool `xml:"noiseCancellation,attr,omitempty"`
	} `xml:"Stream"`
}

// HandlePlivo handles POST /api/v1/plivo/allocate
func (h *AllocateHandler) HandlePlivo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse form data
	if err := r.ParseForm(); err != nil {
		h.logger.Warn("failed to parse Plivo form data", zap.Error(err))
		respondWithPlivoXMLError(w, "invalid form data")
		return
	}

	// Get CallUUID from form data (Plivo uses CallUUID instead of CallSid)
	callUUID := r.FormValue("CallUUID")
	if callUUID == "" {
		respondWithPlivoXMLError(w, "missing CallUUID")
		return
	}

	// Get merchant_id, flow, and template from query parameters
	merchantID := r.URL.Query().Get("merchant_id")
	flow := r.URL.Query().Get("flow")
	if flow == "" {
		flow = "v2"
	}
	template := r.URL.Query().Get("template")

	h.logger.Info("plivo allocation request",
		zap.String("call_uuid", callUUID),
		zap.String("merchant_id", merchantID),
		zap.String("flow", flow),
		zap.String("template", template),
	)

	// Call allocator (use CallUUID as callSID)
	result, err := h.allocator.Allocate(ctx, callUUID, merchantID, "plivo", flow, template)
	if err != nil {
		h.logger.Error("plivo allocation failed",
			zap.Error(err),
			zap.String("call_uuid", callUUID),
		)
		respondWithPlivoXMLError(w, "allocation failed")
		return
	}

	// Build Plivo XML response
	plivoXML := PlivoXMLResponse{}
	plivoXML.Stream.URL = result.WSURL
	plivoXML.Stream.Bidirectional = true
	plivoXML.Stream.KeepCallAlive = true
	plivoXML.Stream.ContentType = "audio/x-mulaw;rate=8000"

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	if err := xml.NewEncoder(w).Encode(plivoXML); err != nil {
		h.logger.Error("failed to encode Plivo XML response", zap.Error(err))
	}
}

// ExotelRequest represents an Exotel webhook request
type ExotelRequest struct {
	CallSID    string `json:"CallSid"`
	MerchantID string `json:"merchant_id,omitempty"`
	Flow       string `json:"flow,omitempty"`
	Template   string `json:"template,omitempty"`
}

// ExotelResponse represents an Exotel JSON response
type ExotelResponse struct {
	URL string `json:"url"`
}

// HandleExotel handles POST /api/v1/exotel/allocate
func (h *AllocateHandler) HandleExotel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse JSON body (Exotel sends JSON)
	var req ExotelRequest
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.logger.Warn("failed to decode Exotel request", zap.Error(err))
		respondWithError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.CallSID == "" {
		respondWithError(w, http.StatusBadRequest, "missing CallSid")
		return
	}

	h.logger.Info("exotel allocation request",
		zap.String("call_sid", req.CallSID),
		zap.String("merchant_id", req.MerchantID),
	)

	// Default flow
	flow := req.Flow
	if flow == "" {
		flow = "v2"
	}

	// Call allocator
	result, err := h.allocator.Allocate(ctx, req.CallSID, req.MerchantID, "exotel", flow, req.Template)
	if err != nil {
		h.logger.Error("exotel allocation failed",
			zap.Error(err),
			zap.String("call_sid", req.CallSID),
		)
		respondWithError(w, http.StatusServiceUnavailable, "allocation failed")
		return
	}

	// Return JSON response with URL
	response := ExotelResponse{
		URL: result.WSURL,
	}

	respondWithJSON(w, http.StatusOK, response)
}

// respondWithJSON sends a JSON response
func respondWithJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("ERROR: failed to encode JSON response: %v", err)
	}
}

// respondWithError sends an error JSON response
func respondWithError(w http.ResponseWriter, status int, message string) {
	respondWithJSON(w, status, map[string]string{"error": message})
}

// respondWithXMLError sends a TwiML error response
func respondWithXMLError(w http.ResponseWriter, message string) {
	twiml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Response>
    <Say>%s</Say>
    <Hangup/>
</Response>`, html.EscapeString(message))
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(twiml))
}

// respondWithPlivoXMLError sends a Plivo XML error response
func respondWithPlivoXMLError(w http.ResponseWriter, message string) {
	plivoXML := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Response>
    <Speak>%s</Speak>
    <Hangup/>
</Response>`, html.EscapeString(message))
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(plivoXML))
}
