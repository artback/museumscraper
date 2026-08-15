// The map: the globe, the museum dots, and the rings that mark venues with
// something on show.

import * as api from "./api.js";
import { prefersReducedMotion, haversine } from "./util.js";
import * as hud from "./hud.js";

// Raster OpenStreetMap tiles, which carry the thing a hand-drawn globe cannot:
// city names, roads, coastlines at every scale, in the local language.
//
// The style is defined here rather than fetched so the application depends on
// exactly one external thing — the tiles — and that one is swappable. A
// deployment with a MapTiler or Stadia key should point TILES at it; the OSM
// community servers are fine for a desktop tool but are not a CDN and their
// usage policy is explicit about that.
//
// One host, not the four lettered ones. Sharding across a.–d. was how HTTP/1.1
// bought parallel downloads; over HTTP/2, which is what these are served on, it
// buys four DNS lookups and four TLS handshakes before the first tile appears.
//
// The @2x tiles are asked for only where they help. They are 512 px images of
// the 256 px tile, so on a hidpi screen they are the sharp version of the same
// tile — but on an ordinary screen they are four times the bytes for nothing.
const RETINA = (window.devicePixelRatio || 1) > 1 ? "@2x" : "";
const TILES = window.MUSEUM_TILES ||
	["https://basemaps.cartocdn.com/dark_all/{z}/{x}/{y}" + RETINA + ".png"];

const TILE_ATTRIBUTION = window.MUSEUM_TILE_ATTRIBUTION ||
	'© <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors, ' +
	'© <a href="https://carto.com/attributions">CARTO</a>';

// Glyphs are what lets the map draw text — without them any layer with a
// text-field is rejected outright, which is what silently stopped the cluster
// labels and every layer declared after them.
const GLYPHS = window.MUSEUM_GLYPHS ||
	"https://demotiles.maplibre.org/font/{fontstack}/{range}.pbf";

const EMPTY = { type: "FeatureCollection", features: [] };

// CAMERA is the shape MapLibre writes a position in: zoom, latitude, longitude,
// optionally followed by bearing and pitch.
const CAMERA = /^-?\d+(?:\.\d+)?\/-?\d+(?:\.\d+)?\/-?\d+(?:\.\d+)?/;

// nameTheCamera brings a link made before the camera had a name up to date.
//
// MapLibre used to write the position as a bare "#14/57.69857/11.9548". The
// hash now carries what is being looked at as well, so the position had to be
// named — and a bare fragment is not a parameter. Read as one it becomes a key
// with an empty value, and the first write after that re-encodes it into the
// URL as "14%2F57.69857%2F11.9548=": no longer a position anything acts on,
// and permanent, because every later write carries it along. Anyone holding a
// link or a bookmark from before has one of those.
//
// Run before the map is built, because the map reads the hash in its
// constructor and this is the only chance to hand it something it understands.
// replaceState rather than pushState: this is a correction to the address, not
// somewhere the reader has been.
function nameTheCamera() {
	const raw = window.location.hash.replace(/^#/, "");
	if (!raw) return;

	const kept = [];
	let camera = null, named = false;

	for (const part of raw.split("&").filter(Boolean)) {
		if (part.startsWith("view=")) {
			named = true;
			kept.push(part);
			continue;
		}
		// The trailing "=" is what URLSearchParams leaves behind when it has
		// already mistaken one of these for a key.
		const decoded = decodeURIComponent(part.replace(/=$/, ""));
		if (!camera && CAMERA.test(decoded)) {
			camera = decoded;
			continue;
		}
		kept.push(part);
	}

	if (camera === null) return;
	// A named position already present wins; the bare one beside it is residue
	// from a page that has since been corrected, and is dropped.
	if (!named) kept.unshift("view=" + camera);
	window.history.replaceState(null, "", "#" + kept.join("&"));
}

nameTheCamera();

export const map = new maplibregl.Map({
	container: "map",
	style: {
		version: 8,
		glyphs: GLYPHS,
		sources: {
			osm: {
				type: "raster",
				tiles: TILES,
				// 256, which is the area the tile actually covers, whichever
				// resolution it was fetched at. It was 512 to stop labels
				// rendering at half scale — but that reported the tile as
				// covering twice the ground, so MapLibre served every view one
				// zoom level coarser than it asked for. Choosing @2x by pixel
				// ratio instead fixes the labels without losing the level.
				tileSize: 256,
				maxzoom: 19,
				attribution: TILE_ATTRIBUTION,
			},
		},
		layers: [
			{ id: "background", type: "background", paint: { "background-color": "#081020" } },
			{ id: "osm", type: "raster", source: "osm" },
		],
		// Declared in the style rather than set afterwards. Calling
		// map.setProjection() straight after the constructor throws, because the
		// style has not loaded yet — and since function declarations hoist, the
		// rest of this file still *looked* defined while the script had in fact
		// stopped at that line, leaving a black page with no error to see.
		projection: { type: "globe" },
		sky: {
			"sky-color": "#0b1c38",
			"horizon-color": "#1b3358",
			// The void is the page's own background, so the globe reads as
			// placed on the page rather than pasted onto it.
			"fog-color": "#05070c",
			"sky-horizon-blend": 0.5,
			"horizon-fog-blend": 0.75,
			"fog-ground-blend": 0.02,
			// Strongest on the first frame, which is the one everybody sees,
			// and faded out gradually: ending at zoom 9 made the glow switch
			// off visibly on the way in.
			"atmosphere-blend": ["interpolate", ["linear"], ["zoom"], 0, 1, 4, 0.6, 7, 0.25, 10, 0],
		},
	},
	center: [10, 30],
	// Not lower: at zoom 1.4 the globe is smaller than the window and sits in a
	// black void while the first tiles arrive, which reads as a broken page.
	zoom: 2.2,
	minZoom: 1.4,
	// The tiles stop at 19, so past 20 the map is upscaled blur that still
	// responds to zooming — which reads as the map having lost its place.
	maxZoom: 20,
	hash: "view",        // the view lives in the URL, so a view can be shared
	attributionControl: { compact: true },
});

// Anything the map itself reports — a refused tile, a bad style — should be
// visible rather than swallowed, since a map that fails silently is just a
// black rectangle.
map.on("error", e => console.error("map:", e && e.error ? e.error.message : e));

// MapLibre measures its container once, at construction, and afterwards only
// watches the window. A container that has no size yet at that moment — a
// stylesheet still arriving, a pane that lays out late — leaves the map at its
// 400×300 fallback: a small globe in the corner of a black page, with the map
// itself working perfectly and nothing on screen to say what is wrong. Watching
// the element rather than the window is what makes that self-correcting.
if (window.ResizeObserver) {
	new ResizeObserver(() => map.resize()).observe(map.getContainer());
}

map.addControl(new maplibregl.NavigationControl({ visualizePitch: true }), "bottom-right");
map.addControl(new maplibregl.ScaleControl({ unit: "metric" }), "bottom-right");

// Geolocation needs a secure context and a permission; where it is unavailable
// MapLibre disables the control itself rather than failing.
export const geolocate = new maplibregl.GeolocateControl({
	positionOptions: { enableHighAccuracy: true },
	trackUserLocation: true,
	showUserLocation: true,
});
map.addControl(geolocate, "bottom-right");

// Adding layers one at a time and reporting failures individually. Declared as
// a list rather than a run of statements because a throw partway through a
// setup function leaves the map half-built with nothing to say so.
function addLayerSafely(spec) {
	try {
		map.addLayer(spec);
	} catch (err) {
		console.error("map: could not add layer " + spec.id + ": " + err.message);
	}
}

/* ---- layers ------------------------------------------------------------- */

export function build({ onPick, onPickVenue }) {
	map.addSource("museums", {
		type: "geojson",
		data: EMPTY,
		// Not clustered. Clustering replaced the map with a few dozen large
		// discs and hid the thing worth looking at — that the museums themselves
		// trace the cities. Forty thousand small circles is a shape the GPU
		// draws in one pass and a person reads at a glance.
	});
	map.addSource("venues", { type: "geojson", data: EMPTY });

	// A wide soft halo under the dots, only at globe scale. Where forty
	// thousand of them overlap this accumulates into a glow over Europe, while
	// a lone museum in Patagonia stays a discrete point.
	addLayerSafely({
		id: "museum-glow", type: "circle", source: "museums", maxzoom: 5.5,
		paint: {
			"circle-color": "#ffc477",
			"circle-radius": ["interpolate", ["exponential", 1.3], ["zoom"], 1.4, 5, 3, 7, 5.5, 9],
			"circle-blur": 1,
			// Faint on purpose. Pushed higher, the cities of western Europe
			// merge into one unbroken sheet of light and the shape that makes
			// the view worth looking at — which cities, and how far apart —
			// disappears into it.
			"circle-opacity": ["interpolate", ["linear"], ["zoom"], 1.4, 0.05, 4, 0.035, 5.4, 0],
		},
	});

	addLayerSafely({
		id: "museum-points", type: "circle", source: "museums",
		paint: {
			// Paler at globe scale, where it reads as emitted light; full amber
			// at street level, where it reads as a placed marker.
			"circle-color": ["interpolate", ["linear"], ["zoom"], 2, "#ffc477", 8, "#ffb454"],
			// Exponential rather than linear: apparent size should track the
			// geometric nature of zoom, or the middle zooms look lumpy.
			"circle-radius": ["interpolate", ["exponential", 1.35], ["zoom"],
				1.4, 0.8, 4, 1.9, 8, 3.6, 12, 5.5, 16, 8],
			// A hair of blur below zoom 6 stops sub-pixel dots aliasing into
			// squares, and is what lets overlap read as density rather than mush.
			"circle-blur": ["interpolate", ["linear"], ["zoom"], 1.4, 0.35, 6, 0.15, 10, 0],
			// Small and faint at globe scale, so that overlap accumulates into a
			// gradient instead of a ceiling.
			//
			// These are forty thousand marks on a sphere a few hundred pixels
			// across, and at the opacity a single readable dot wants they stop
			// being marks at all: western Europe went to a flat sheet of colour
			// from Ireland to Poland, taking the coastlines, the borders and the
			// city names under it with it. Saturation has to mean "this is the
			// densest place there is" — Paris, the Rhine — rather than "more than
			// a few", or the view says the same thing about a continent that it
			// says about one street.
			"circle-opacity": ["interpolate", ["linear"], ["zoom"],
				1.4, 0.18, 2.5, 0.3, 4, 0.5, 6, 0.9, 9, 1],
			"circle-stroke-width": ["interpolate", ["linear"], ["zoom"], 7, 0, 10, 1, 14, 1.5],
			"circle-stroke-color": "#05070c",
			"circle-stroke-opacity": 0.85,
		},
	});

	// A ring per venue with something on show, drawn from the exhibitions'
	// own coordinates. Cyan rather than a second warm: amber against cyan
	// survives both common forms of colour blindness, so "has something on"
	// stays legible as a difference rather than only as a hue.
	addLayerSafely({
		id: "venue-rings", type: "circle", source: "venues",
		paint: {
			"circle-color": "#7fd4e3",
			"circle-opacity": 0.1,
			"circle-radius": ["interpolate", ["exponential", 1.3], ["zoom"], 6, 7, 12, 13, 16, 20],
			"circle-stroke-width": 1.5,
			"circle-stroke-color": "#7fd4e3",
			"circle-stroke-opacity": ["case", ["boolean", ["feature-state", "hover"], false], 1, 0.65],
		},
	});

	// The halo for whichever museum is open: a soft glow under a warm ring,
	// both scaled with zoom. A fixed 13px radius was nearly filled by the dot
	// itself at zoom 16, where it stopped reading as a halo at all.
	addLayerSafely({
		id: "museum-selected-glow", type: "circle", source: "museums",
		filter: ["==", ["get", "id"], -1],
		paint: {
			"circle-color": "#ffb454",
			"circle-radius": ["interpolate", ["exponential", 1.3], ["zoom"], 4, 14, 10, 22, 16, 34],
			"circle-blur": 0.9, "circle-opacity": 0.18,
		},
	});
	addLayerSafely({
		id: "museum-selected", type: "circle", source: "museums",
		filter: ["==", ["get", "id"], -1],
		paint: {
			"circle-color": "transparent",
			"circle-radius": ["interpolate", ["exponential", 1.3], ["zoom"], 4, 7, 8, 10, 12, 14, 16, 20],
			"circle-stroke-width": ["interpolate", ["linear"], ["zoom"], 4, 1.5, 10, 2, 16, 2.5],
			"circle-stroke-color": "#fff5e6",
			"circle-stroke-opacity": 0.9,
		},
	});

	// An invisible layer whose only job is to be big enough to hit. The drawn
	// dots are between 1.5 and 8 pixels across and MapLibre hit-tests the
	// geometry it drew, so on a touchscreen the visible marker is far smaller
	// than a fingertip and picking a particular museum is mostly luck.
	addLayerSafely({
		id: "museum-hits", type: "circle", source: "museums",
		paint: { "circle-color": "#000", "circle-opacity": 0, "circle-radius": 14 },
	});

	wirePicking(onPick, onPickVenue);
}

function wirePicking(onPick, onPickVenue) {
	for (const layer of ["museum-hits", "venue-rings"]) {
		map.on("mouseenter", layer, () => map.getCanvas().style.cursor = "pointer");
		map.on("mouseleave", layer, () => map.getCanvas().style.cursor = "");
	}

	map.on("click", "museum-hits", e => {
		// Overlapping dots used to always resolve to the same museum, leaving
		// the others unreachable however far you zoomed.
		const ids = [...new Set(e.features.map(f => f.properties.id))];
		onPick(ids, e.lngLat);
	});
	map.on("click", "venue-rings", e => onPickVenue(e.features[0].properties));

	wireHover();
}

// A name on hover.
//
// This used to read `properties.name` off the hovered feature, which /v1/points
// has never sent — it packs each museum as [id, lat, lon] and nothing more — so
// the handler read undefined and returned every time. The popup had never once
// appeared; what remained was the cost of asking, on every pointer movement.
//
// The name is fetched instead, after a pause and once per museum. The pause is
// what makes it affordable: crossing a dense city centre passes over dozens of
// dots, and none of them was the one being asked about.
function wireHover() {
	const popup = new maplibregl.Popup({ closeButton: false, closeOnClick: false, offset: 12 });
	const names = new Map();
	let hovering = null, timer = null;

	map.on("mousemove", "museum-hits", e => {
		const feature = e.features[0];
		const id = feature.properties.id;
		if (id === hovering) return;

		hovering = id;
		clearTimeout(timer);
		popup.remove();

		// The coordinate is nudged onto the same copy of the world as the
		// cursor, or a popup near the date line attaches itself to a dot one
		// full turn away.
		const at = feature.geometry.coordinates.slice();
		while (Math.abs(e.lngLat.lng - at[0]) > 180) at[0] += e.lngLat.lng > at[0] ? 360 : -360;

		const show = name => {
			if (hovering !== id || !name) return;
			popup.setLngLat(at).setText(name).addTo(map);
		};

		if (names.has(id)) {
			show(names.get(id));
			return;
		}
		timer = setTimeout(async () => {
			const result = await api.museum(id);
			const name = result.ok ? result.data.name : "";
			names.set(id, name);
			show(name);
		}, 140);
	});

	map.on("mouseleave", "museum-hits", () => {
		hovering = null;
		clearTimeout(timer);
		popup.remove();
	});
}

/* ---- the museum points -------------------------------------------------- */

let inFlight = null, lastBox = null, lastTruncated = true;

// The visible box, or nothing when the whole globe is in view.
//
// A view straddling the date line is asked for globally rather than by box.
// Wrapped bounds arrive as west 170, east -170, which is a box the server reads
// as inverted and answers with nothing; unwrapped bounds arrive as west 170,
// east 190, and the server clamps the east edge to 180 and quietly drops half
// the view. Either way museums went missing with nothing to say so, and the
// whole-world query costs little for what is almost entirely ocean.
function visibleBox() {
	if (map.getZoom() < 2.5) return null;
	try {
		const b = map.getBounds();
		const s = b.getSouth(), n = b.getNorth();
		let w = b.getWest(), e = b.getEast();
		if (![w, s, e, n].every(Number.isFinite)) return null;

		if (e < w) e += 360;
		if (e - w >= 355) return null;      // effectively the whole world
		if (w < -180 || e > 180) return null; // crosses the date line

		// Padded, because under the globe projection the visible region is a
		// spherical cap rather than a rectangle and the corners under-report.
		const padX = (e - w) * 0.08, padY = (n - s) * 0.08;
		return [
			Math.max(w - padX, -180), Math.max(s - padY, -90),
			Math.min(e + padX, 180), Math.min(n + padY, 90),
		];
	} catch (_) {
		return null;
	}
}

function inside(inner, outer) {
	if (!inner || !outer) return false;
	return inner[0] >= outer[0] && inner[1] >= outer[1] &&
		inner[2] <= outer[2] && inner[3] <= outer[3];
}

export async function loadPoints() {
	const box = visibleBox();

	// A small pan inside an area already fully loaded needs no request at all.
	// Only sound when the last answer was complete: a truncated one was the
	// most notable of that area, so a closer look really does have more to say.
	if (!lastTruncated && inside(box, lastBox)) return;

	// Superseded requests are cancelled rather than merely ignored. The token
	// guard that used to be here still drew the right thing, but a megabyte of
	// points for a view nobody is looking at was downloaded in full first —
	// which on a phone is most of what the page costs.
	if (inFlight) inFlight.abort();
	const controller = new AbortController();
	inFlight = controller;

	const result = await api.points(box, controller.signal);
	if (controller !== inFlight) return;
	inFlight = null;

	if (!result.ok) {
		if (!result.aborted) hud.failed(result.error, loadPoints);
		return;
	}

	const source = map.getSource("museums");
	if (!source) return;

	source.setData({
		type: "FeatureCollection",
		features: result.data.points.map(p => ({
			type: "Feature",
			geometry: { type: "Point", coordinates: [p[2], p[1]] },
			properties: { id: p[0] },
		})),
	});

	lastBox = box;
	lastTruncated = Boolean(result.data.truncated);
	hud.museums(result.data.count, result.data.truncated);
}

/* ---- venues with something on show -------------------------------------- */

// showVenues draws one ring per place with a listing. Built from the
// exhibitions' own coordinates, so it needs neither an identifier nor a name
// match to be correct about where things are on.
export function showVenues(shows) {
	const source = map.getSource("venues");
	if (!source) return;

	const byPlace = new Map();
	for (const show of shows) {
		if (!Number.isFinite(show.latitude) || (!show.latitude && !show.longitude)) continue;
		const key = show.latitude.toFixed(4) + "," + show.longitude.toFixed(4);
		const seen = byPlace.get(key);
		if (seen) seen.count += 1;
		else byPlace.set(key, { name: show.museum, count: 1, lat: show.latitude, lon: show.longitude });
	}

	source.setData({
		type: "FeatureCollection",
		features: [...byPlace.values()].map(v => ({
			type: "Feature",
			geometry: { type: "Point", coordinates: [v.lon, v.lat] },
			properties: { name: v.name, count: v.count },
		})),
	});
}

export function clearVenues() {
	map.getSource("venues")?.setData(EMPTY);
}

/* ---- camera ------------------------------------------------------------- */

export function select(id) {
	for (const layer of ["museum-selected", "museum-selected-glow"]) {
		if (map.getLayer(layer)) {
			map.setFilter(layer, ["==", ["get", "id"], id === null ? -1 : Number(id)]);
		}
	}
}

// goTo moves the camera.
//
// flyTo animates with requestAnimationFrame, which a browser does not run for a
// hidden or backgrounded page — the camera then simply never arrives. Falling
// back to an immediate move keeps the destination correct when the animation
// cannot play, and respects a reader who has asked for less motion.
export function goTo(options) {
	if (document.hidden || prefersReducedMotion()) map.jumpTo(options);
	else map.flyTo({ ...options, speed: 1.3 });
}

// reveal moves only when it has to. Opening each of five candidates from a list
// used to re-fly the camera five times, throwing away the overview that made
// them comparable — and each move refetched the points and re-armed a scrape.
export function reveal(lon, lat, minZoom = 14) {
	if (!Number.isFinite(lat) || (!lat && !lon)) return;

	const bounds = map.getBounds();
	const margin = (bounds.getEast() - bounds.getWest()) * 0.12;
	const visible = lon >= bounds.getWest() + margin && lon <= bounds.getEast() - margin &&
		lat >= bounds.getSouth() + margin && lat <= bounds.getNorth() - margin;

	if (visible) return;
	goTo({ center: [lon, lat], zoom: Math.max(map.getZoom(), minZoom) });
}

// here describes what is on screen: a centre, and the radius that covers it —
// half the diagonal, which is what reaches the corners of the visible rectangle.
export function here() {
	const c = map.getCenter();
	const b = map.getBounds();
	const radiusKm = Math.max(
		haversine(c.lat, c.lng, b.getNorth(), b.getEast()),
		haversine(c.lat, c.lng, b.getSouth(), b.getWest()));
	return { lat: c.lat, lon: c.lng, radiusKm };
}
