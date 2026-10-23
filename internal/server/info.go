package server

import "net/http"

// InfoOptions are what GET /api/v1/info reports.
type InfoOptions struct{}

// InfoHandler serves GET /api/v1/info.
type InfoHandler struct{}

// NewInfoHandler builds the handler.
func NewInfoHandler(InfoOptions) *InfoHandler { return &InfoHandler{} }

func (h *InfoHandler) get(w http.ResponseWriter, r *http.Request) {}
