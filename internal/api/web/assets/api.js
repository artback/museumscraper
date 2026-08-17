// Every call to the catalogue, in one place.
//
// Gathered here because the limits are the server's, not this page's, and they
// were previously spread across the call sites and drifted: the map asked to
// scrape a 60 km circle at a time when the API had rejected anything over 50 km
// for as long as it had existed, and every one of those requests was a 400 that
// the page then swallowed.

import { getJSON } from "./util.js";

// MAX_RADIUS_KM mirrors maxRadiusKm in internal/api/api.go. A request over it is
// refused outright, so it is a bound to respect before asking rather than an
// error to report afterwards.
export const MAX_RADIUS_KM = 50;

// MAX_EXHIBITIONS mirrors the server's limit cap. Asking for exactly the cap is
// how the page learns that there were more: the API reports no total for
// exhibitions, so a full page is the only available signal that it truncated.
export const MAX_EXHIBITIONS = 500;

export function clampRadius(km) {
	if (!Number.isFinite(km)) return 1;
	return Math.max(1, Math.min(km, MAX_RADIUS_KM));
}

function area({ lat, lon, radiusKm }, extra = {}) {
	return new URLSearchParams({
		lat: lat.toFixed(5),
		lon: lon.toFixed(5),
		radius_km: clampRadius(radiusKm).toFixed(1),
		...extra,
	});
}

export function points(bbox, signal) {
	const query = "limit=40000" + (bbox ? "&bbox=" + bbox.map(v => v.toFixed(4)).join(",") : "");
	return getJSON("/v1/points?" + query, { signal });
}

export function museum(id, signal) {
	return getJSON("/v1/museums/" + encodeURIComponent(id), { signal });
}

export function museumsNear(spot, limit = 200, signal) {
	return getJSON("/v1/museums?" + area(spot, { limit: String(limit) }), { signal });
}

// Exhibitions are asked for with upcoming=true, which means running and
// not-yet-open together rather than only the future. The rows carry the flags
// that tell those apart, so one request answers both questions.
export function exhibitionsNear(spot, signal) {
	const params = area(spot, { upcoming: "true", limit: String(MAX_EXHIBITIONS) });
	return getJSON("/v1/exhibitions?" + params, { signal });
}

export function places(term, signal) {
	return getJSON("/v1/places?q=" + encodeURIComponent(term), { signal });
}

export function search(term, limit = 10, signal) {
	return getJSON("/v1/search?q=" + encodeURIComponent(term) + "&limit=" + limit, { signal });
}

// searchExhibitions finds a show by its name, anywhere. The title is often the
// only thing somebody knows — not the museum holding it, and not the city.
export function searchExhibitions(term, limit = 6, signal) {
	return getJSON("/v1/exhibitions?q=" + encodeURIComponent(term) + "&limit=" + limit, { signal });
}

// scrapeStatus asks what is happening somewhere without starting anything: a
// GET, so opening a museum never sets other people's websites being read.
export function scrapeStatus(spot) {
	return getJSON("/v1/scrape?" + area(spot));
}

export function startScrape(spot) {
	return getJSON("/v1/scrape?" + area(spot), { method: "POST" });
}

// running is true while an area is queued or being read — the two states worth
// reporting, and the two worth asking about again.
export function running(status) {
	return Boolean(status) && (status.state === "queued" || status.state === "running");
}
