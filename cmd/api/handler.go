package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"museum/internal/repository"

	"github.com/jackc/pgx/v5"
)

type Handler struct {
	museumRepo     *repository.MuseumRepository
	exhibitionRepo *repository.ExhibitionRepository
}

func NewHandler(museumRepo *repository.MuseumRepository, exhibitionRepo *repository.ExhibitionRepository) *Handler {
	return &Handler{
		museumRepo:     museumRepo,
		exhibitionRepo: exhibitionRepo,
	}
}

// JSON response helpers

type listResponse struct {
	Data  any `json:"data"`
	Count int `json:"count"`
}

type itemResponse struct {
	Data any `json:"data"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode JSON response", "error", err)
	}
}

func writeList(w http.ResponseWriter, data any, count int) {
	writeJSON(w, http.StatusOK, listResponse{Data: data, Count: count})
}

func writeItem(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, itemResponse{Data: data})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// Parameter parsing helpers

func queryInt(r *http.Request, key string, defaultVal int) (int, error) {
	s := r.URL.Query().Get(key)
	if s == "" {
		return defaultVal, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return v, nil
}

func queryFloat(r *http.Request, key string) (float64, bool, error) {
	s := r.URL.Query().Get(key)
	if s == "" {
		return 0, false, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}

// Handlers

func (h *Handler) ListMuseums(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 50)
	if err != nil || limit < 1 || limit > 500 {
		writeError(w, http.StatusBadRequest, "invalid limit parameter (must be 1-500)")
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "invalid offset parameter")
		return
	}

	museums, total, err := h.museumRepo.List(r.Context(), limit, offset)
	if err != nil {
		slog.Error("failed to list museums", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if museums == nil {
		museums = []repository.Museum{}
	}
	writeJSON(w, http.StatusOK, struct {
		Data   []repository.Museum `json:"data"`
		Count  int                 `json:"count"`
		Total  int                 `json:"total"`
		Limit  int                 `json:"limit"`
		Offset int                 `json:"offset"`
	}{
		Data:   museums,
		Count:  len(museums),
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

func (h *Handler) GetMuseum(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid museum ID")
		return
	}

	museum, err := h.museumRepo.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "museum not found")
			return
		}
		slog.Error("failed to get museum", "error", err, "id", id)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// Fetch active exhibitions for this museum.
	exhibitions, err := h.exhibitionRepo.FindActiveByMuseum(r.Context(), id)
	if err != nil {
		slog.Error("failed to get exhibitions", "error", err, "museum_id", id)
	} else {
		museum.Exhibitions = exhibitions
	}

	writeItem(w, museum)
}

func (h *Handler) SearchMuseums(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "query parameter 'q' is required")
		return
	}
	limit, err := queryInt(r, "limit", 20)
	if err != nil || limit < 1 || limit > 100 {
		writeError(w, http.StatusBadRequest, "invalid limit parameter (must be 1-100)")
		return
	}

	museums, err := h.museumRepo.SearchByName(r.Context(), q, limit)
	if err != nil {
		slog.Error("failed to search museums", "error", err, "query", q)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if museums == nil {
		museums = []repository.Museum{}
	}
	writeList(w, museums, len(museums))
}

func (h *Handler) NearbyMuseums(w http.ResponseWriter, r *http.Request) {
	lat, hasLat, err := queryFloat(r, "lat")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid lat parameter")
		return
	}
	lon, hasLon, err := queryFloat(r, "lon")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid lon parameter")
		return
	}
	if !hasLat || !hasLon {
		writeError(w, http.StatusBadRequest, "lat and lon parameters are required")
		return
	}
	if lat < -90 || lat > 90 {
		writeError(w, http.StatusBadRequest, "lat must be between -90 and 90")
		return
	}
	if lon < -180 || lon > 180 {
		writeError(w, http.StatusBadRequest, "lon must be between -180 and 180")
		return
	}

	radius, _, err := queryFloat(r, "radius")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid radius parameter")
		return
	}
	if radius <= 0 {
		radius = 5000 // default 5km
	}
	if radius > 100000 {
		writeError(w, http.StatusBadRequest, "radius must not exceed 100000 meters")
		return
	}

	limit, err := queryInt(r, "limit", 50)
	if err != nil || limit < 1 || limit > 200 {
		writeError(w, http.StatusBadRequest, "invalid limit parameter (must be 1-200)")
		return
	}

	museums, err := h.museumRepo.FindNearby(r.Context(), lat, lon, radius, limit)
	if err != nil {
		slog.Error("failed to find nearby museums", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if museums == nil {
		museums = []repository.Museum{}
	}
	writeList(w, museums, len(museums))
}

func (h *Handler) MuseumsByCity(w http.ResponseWriter, r *http.Request) {
	city := r.PathValue("city")
	if city == "" {
		writeError(w, http.StatusBadRequest, "city parameter is required")
		return
	}

	museums, err := h.museumRepo.FindByCity(r.Context(), city)
	if err != nil {
		slog.Error("failed to get museums by city", "error", err, "city", city)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if museums == nil {
		museums = []repository.Museum{}
	}
	writeList(w, museums, len(museums))
}

func (h *Handler) MuseumsByCountry(w http.ResponseWriter, r *http.Request) {
	country := r.PathValue("country")
	if country == "" {
		writeError(w, http.StatusBadRequest, "country parameter is required")
		return
	}

	museums, err := h.museumRepo.FindByCountry(r.Context(), country)
	if err != nil {
		slog.Error("failed to get museums by country", "error", err, "country", country)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if museums == nil {
		museums = []repository.Museum{}
	}
	writeList(w, museums, len(museums))
}

func (h *Handler) ExhibitionsByCity(w http.ResponseWriter, r *http.Request) {
	city := r.PathValue("city")
	if city == "" {
		writeError(w, http.StatusBadRequest, "city parameter is required")
		return
	}

	exhibitions, err := h.exhibitionRepo.FindActiveInCity(r.Context(), city)
	if err != nil {
		slog.Error("failed to get exhibitions by city", "error", err, "city", city)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if exhibitions == nil {
		exhibitions = []repository.ExhibitionWithMuseum{}
	}
	writeList(w, exhibitions, len(exhibitions))
}

func (h *Handler) ExhibitionsNearby(w http.ResponseWriter, r *http.Request) {
	lat, hasLat, err := queryFloat(r, "lat")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid lat parameter")
		return
	}
	lon, hasLon, err := queryFloat(r, "lon")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid lon parameter")
		return
	}
	if !hasLat || !hasLon {
		writeError(w, http.StatusBadRequest, "lat and lon parameters are required")
		return
	}
	if lat < -90 || lat > 90 {
		writeError(w, http.StatusBadRequest, "lat must be between -90 and 90")
		return
	}
	if lon < -180 || lon > 180 {
		writeError(w, http.StatusBadRequest, "lon must be between -180 and 180")
		return
	}

	radius, _, err := queryFloat(r, "radius")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid radius parameter")
		return
	}
	if radius <= 0 {
		radius = 5000
	}
	if radius > 100000 {
		writeError(w, http.StatusBadRequest, "radius must not exceed 100000 meters")
		return
	}

	limit, err := queryInt(r, "limit", 50)
	if err != nil || limit < 1 || limit > 200 {
		writeError(w, http.StatusBadRequest, "invalid limit parameter (must be 1-200)")
		return
	}

	exhibitions, err := h.exhibitionRepo.FindActiveNearby(r.Context(), lat, lon, radius, limit)
	if err != nil {
		slog.Error("failed to find nearby exhibitions", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if exhibitions == nil {
		exhibitions = []repository.ExhibitionWithMuseum{}
	}
	writeList(w, exhibitions, len(exhibitions))
}

func (h *Handler) HealthCheck(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
