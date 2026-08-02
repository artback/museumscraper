// Wiring: what happens when, and what the URL remembers.

import * as globe from "./globe.js";
import * as api from "./api.js";
import * as scrape from "./scrape.js";
import * as search from "./search.js";
import * as area from "./area.js";
import * as museum from "./museum.js";
import * as hud from "./hud.js";
import { mount, closeTop, rememberFocus } from "./dock.js";
import { el, zoomForRadius } from "./util.js";

// Order is priority: the museum sits above the area it came from, and the area
// stays behind it rather than being destroyed. Closing a museum used to leave
// nowhere to go back to, so the way back to "what is in Gothenburg" was to type
// Gothenburg again.
mount(document.getElementById("dock"), museum.card, area.card);

/* ---- the map ------------------------------------------------------------ */

let moveTimer = null;

globe.map.on("load", () => {
	globe.build({ onPick: pickMuseum, onPickVenue: props => area.openVenue(props) });

	// The canvas is a picture as far as assistive technology is concerned, so
	// it says where the equivalent is rather than pretending to be readable.
	globe.map.getCanvas().setAttribute("aria-label",
		"Map of museums. Search for a place to get a list of the museums in it.");

	hud.loading();
	globe.loadPoints();

	// One handler for "the view settled", debounced. Zooming fires moveend for
	// every notch of the wheel, and asking the server on each one put dozens of
	// requests in flight that nothing would ever read.
	globe.map.on("moveend", () => {
		clearTimeout(moveTimer);
		moveTimer = setTimeout(() => {
			globe.loadPoints();
			offerToLook();
		}, 200);
	});
});

// Overlapping dots used to resolve to whichever the map happened to list first,
// leaving the others unreachable however far you zoomed in.
function pickMuseum(ids, at) {
	if (ids.length === 1) museum.show(ids[0]);
	else chooseAmong(ids, at);
}

async function chooseAmong(ids, at) {
	const found = await Promise.all(ids.slice(0, 8).map(id => api.museum(id)));
	const named = found.filter(result => result.ok).map(result => result.data);
	if (!named.length) return;
	if (named.length === 1) { museum.show(named[0].id); return; }

	const popup = new maplibregl.Popup({ offset: 12, maxWidth: "260px" });
	popup.setLngLat(at).setDOMContent(el("div", {}, [
		el("div", { class: "meta" }, named.length + " museums here"),
		el("ul", { class: "rows" }, named.map(hit => el("li", {}, el("button", {
			class: "row", type: "button",
			onclick: () => { popup.remove(); museum.show(hit.id); },
		}, el("span", { class: "row__name" }, hit.name))))),
	])).addTo(globe.map);
}

/* ---- looking for exhibitions -------------------------------------------- */

const pill = document.getElementById("look");

// The scrape used to fire on its own whenever the view settled below a certain
// size. It is a request to read strangers' websites, so it is offered rather
// than taken — which is also the only way somebody learns the feature exists,
// and the only way to ask again after a refusal.
function offerToLook() {
	const spot = globe.here();
	pill.hidden = !(scrape.worthScraping(spot, globe.map.getZoom()) && !scrape.alreadyAsked(spot));
}

pill.addEventListener("click", async () => {
	const spot = globe.here();
	pill.hidden = true;

	// Reading an area is what the area card is for, so opening it is how the
	// progress and the results get somewhere to appear.
	if (!area.area()) {
		await area.show({
			name: "This area",
			latitude: spot.lat,
			longitude: spot.lon,
			radius_km: api.clampRadius(spot.radiusKm),
		});
	}
	area.look();
});

/* ---- near you ----------------------------------------------------------- */

// Only when somebody asks for it: trackuserlocationstart fires on the press,
// where geolocate fires again for every position update while tracking.
let wantsNearby = false;

globe.geolocate.on("trackuserlocationstart", () => { wantsNearby = true; });
globe.geolocate.on("geolocate", position => {
	if (!wantsNearby) return;
	wantsNearby = false;
	area.show({
		name: "Near you",
		latitude: position.coords.latitude,
		longitude: position.coords.longitude,
		radius_km: 5,
	});
});

/* ---- search ------------------------------------------------------------- */

search.wire({
	onPlace: place => {
		rememberFocus();
		museum.card.close();
		globe.goTo({
			center: [place.longitude, place.latitude],
			zoom: zoomForRadius(place.radius_km),
		});
		area.show(place);
	},
	onMuseum: id => museum.show(id),

	// A show found by name is opened at its venue rather than as a link off the
	// page: the venue is where its dates, its address and the rest of its
	// programme are. /v1/museums/{id} takes a Wikidata id as readily as a
	// numeric one, so the exhibition's own reference is enough to open it; the
	// name is the fallback for the listings that carry no reference. Either
	// way the map goes there, so a venue that resolves to no record at all
	// still leaves you looking at the right place.
	onShow: show => {
		globe.reveal(show.longitude, show.latitude, 15);
		if (show.museum_wikidata_id) museum.show(show.museum_wikidata_id);
		else area.openVenue({ name: show.museum });
	},
});

area.onOpenMuseum(id => museum.show(id));

/* ---- the URL ------------------------------------------------------------ */

// The camera has always been in the URL; what was being looked at was not. A
// museum could not be linked to, did not survive a reload, and the back button
// did nothing — while the API's own documentation calls the museum id the thing
// to deep link to.
museum.onOpenChange(open => setHash("m", open ? String(open.id) : null));

function hashParams() {
	return new URLSearchParams(location.hash.replace(/^#/, ""));
}

function setHash(key, value) {
	const params = hashParams();
	if (params.get(key) === (value ?? null)) return;
	if (value === null) params.delete(key);
	else params.set(key, value);
	// pushState, so closing the museum is what the back button undoes.
	history.pushState(null, "", "#" + params.toString());
}

// A link to a museum opens it straight away, without waiting for the map.
//
// This used to run inside the map's load handler, which tied a shared link to
// the globe finishing: a slow tile host, a refused style, a browser that will
// not run animation frames for a background tab, and the card never appeared
// even though the record it names was one request away. Selecting and
// revealing are both no-ops until the layers exist, so there is nothing here
// that needs the map to be ready.
function restoreFromURL() {
	const id = hashParams().get("m");
	if (id) museum.show(id);
}

restoreFromURL();

window.addEventListener("popstate", () => {
	const id = hashParams().get("m");
	const open = museum.current();
	if (id && String(open?.id) !== id) museum.show(id);
	else if (!id && open) museum.card.close();
});

/* ---- keyboard ----------------------------------------------------------- */

document.addEventListener("keydown", e => {
	if (e.defaultPrevented) return;

	// Escape closes the innermost card rather than everything, so leaving a
	// museum leaves the list it came from showing. It used to be handled only
	// inside the search box, so once anything else had focus there was no way
	// to close a panel from the keyboard at all.
	if (e.key === "Escape") {
		if (closeTop()) e.preventDefault();
		return;
	}

	const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName || "");
	if (typing || e.metaKey || e.ctrlKey || e.altKey) return;

	if (e.key === "/") {
		e.preventDefault();
		search.focus();
	}
});

/* ---- the first visit ---------------------------------------------------- */

// A dark globe of orange dots beside a box saying "search museums" tells nobody
// that the box also takes city names, that the dots open, or that a city's
// listings can be read on request.
const HINT_SEEN = "museum.hint";

function hint() {
	let seen = false;
	try { seen = Boolean(localStorage.getItem(HINT_SEEN)); } catch (_) { seen = true; }
	if (seen) return;

	const node = el("div", { class: "hint" }, [
		el("span", {}, "Search a city to see what's on, or click any dot."),
		el("button", {
			class: "icon-btn", type: "button", "aria-label": "Dismiss hint",
			onclick: () => {
				try { localStorage.setItem(HINT_SEEN, "1"); } catch (_) { /* private mode */ }
				node.remove();
			},
		}, "×"),
	]);
	document.getElementById("search").after(node);
}

hint();
