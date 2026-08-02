// The card for a place: the museums in it, and what is on show there.
//
// Searching a city should answer "what is here", not move the camera and leave
// somebody to find the dots themselves. Two tabs rather than two panels,
// because they are two answers to one question and only one of them is wanted
// at a time.

import * as api from "./api.js";
import * as globe from "./globe.js";
import * as scrape from "./scrape.js";
import * as shows from "./exhibitions.js";
import * as hud from "./hud.js";
import { Card } from "./dock.js";
import { el, clear, plural } from "./util.js";

export const card = new Card({ modifier: "card--area", onClose: () => {
	globe.clearVenues();
	scrape.stop("area");
	current = null;
} });

let current = null;   // { name, spot }
let museums = [];
let listings = [];
let report = null;
let tab = "museums";
let filter = "now";
let sort = "closing";
let openMuseum = null; // set by app.js, so a row can open the detail card

export function onOpenMuseum(handler) {
	openMuseum = handler;
}

/* ---- loading ------------------------------------------------------------ */

export async function show(place) {
	const spot = {
		lat: Number(place.latitude),
		lon: Number(place.longitude),
		radiusKm: api.clampRadius(place.radius_km || 15),
	};
	current = { name: String(place.name || "").split(",")[0], spot };
	tab = "museums";
	// A different place has not been looked at, whatever happened at the last one.
	looked = null;

	card.setTitle(current.name).open().busy("Looking…");

	// Both at once: the museums here, and everything on show, so each museum
	// can say how much of it is theirs.
	const [nearby, onShow] = await Promise.all([
		api.museumsNear(spot),
		api.exhibitionsNear(spot),
	]);
	if (!current || current.spot !== spot) return; // a newer place is showing

	if (!nearby.ok) {
		card.failed(nearby.error || "Could not load this area.", () => show(place));
		return;
	}

	museums = nearby.data.museums || [];
	listings = onShow.ok ? (onShow.data.exhibitions || []) : [];
	report = onShow.ok ? onShow.data.coverage : null;

	globe.showVenues(listings);
	hud.summarise(current.name, nearby.data.total);
	paint();
}

/* ---- drawing ------------------------------------------------------------ */

function paint() {
	if (!current) return;
	const body = clear(card.body);

	body.append(tabs());
	body.append(tab === "museums" ? museumList() : showList());
}

function tabs() {
	const counts = countsByVenue();
	const total = listings.length;

	return el("div", { class: "tabs", role: "tablist" }, [
		tabButton("museums", "Museums", museums.length, counts.size > 0),
		tabButton("shows", "On show", total, total > 0),
	]);
}

function tabButton(name, label, count, highlight) {
	return el("button", {
		class: "tab" + (tab === name ? " tab--on" : ""),
		type: "button",
		role: "tab",
		"aria-selected": String(tab === name),
		onclick: () => { tab = name; paint(); },
	}, [
		label,
		count ? el("span", {
			class: "badge" + (name === "shows" && highlight ? " badge--shows" : ""),
			text: String(count),
		}) : null,
	]);
}

// countsByVenue counts listings per museum.
//
// Keyed on the Wikidata id where there is one. Matching on the venue name is
// what the page used to do everywhere, and it is wrong in both directions: a
// re-scrape or a name merge renames one side and the museum silently shows
// nothing, while two museums sharing a name within the radius take each other's
// listings.
function countsByVenue() {
	const counts = new Map();
	for (const show of listings) {
		const key = show.museum_wikidata_id || normalise(show.museum);
		if (!key) continue;
		counts.set(key, (counts.get(key) || 0) + 1);
	}
	return counts;
}

function normalise(name) {
	return String(name ?? "").normalize("NFC").trim().toLowerCase().replace(/^the\s+/, "");
}

function keyFor(museum) {
	return museum.wikidata_id || normalise(museum.name);
}

function museumList() {
	if (!museums.length) {
		return el("div", { class: "empty", "data-glyph": "◎" }, [
			"No museums within " + Math.round(current.spot.radiusKm) + " km",
			el("small", {}, "Try a wider view."),
		]);
	}

	const counts = countsByVenue();

	return el("div", {}, [
		el("div", { class: "coverage" }, [
			plural(museums.length, "museum") + " here",
			el("span", { class: "meta" }, " · nearest first"),
		]),
		el("ul", { class: "rows" }, museums.map(museum => {
			const count = counts.get(keyFor(museum)) || 0;
			return el("li", {}, el("button", {
				class: "row", type: "button",
				onclick: () => openMuseum?.(museum.id),
			}, [
				el("span", { class: "row__name" }, [
					museum.name,
					count ? el("span", { class: "badge badge--shows" }, [
						String(count),
						el("span", { class: "sr" }, " exhibitions on"),
					]) : null,
				]),
				el("span", { class: "meta" },
					museum.distance_km.toFixed(1) + " km" +
					(museum.locality ? " · " + museum.locality : "")),
			]));
		})),
	]);
}

function showList() {
	const out = el("div", {});

	const kept = listings.filter(shows.FILTERS[filter].keep);
	const summary = shows.coverage(report, listings, {
		scraping: scraping,
		looked: looked,
		onScrape: () => look(),
	});

	if (summary) out.append(summary);
	if (progressBox) out.append(progressBox);

	if (listings.length) {
		out.append(controls(kept.length));
		const list = shows.render(kept, { sort, onPick: pickVenue });
		out.append(list || el("div", { class: "empty", "data-glyph": "◎" }, [
			"Nothing matches " + shows.FILTERS[filter].label.toLowerCase(),
			el("small", {}, "Try another period."),
		]));
	}
	return out;
}

function controls(count) {
	return el("div", { class: "controls" }, [
		el("div", { class: "chips", role: "group", "aria-label": "When" },
			Object.entries(shows.FILTERS).map(([key, spec]) => el("button", {
				class: "chip" + (filter === key ? " chip--on" : ""),
				type: "button",
				"aria-pressed": String(filter === key),
				onclick: () => { filter = key; paint(); },
			}, spec.label))),

		el("div", { class: "controls__sort" }, [
			el("label", { class: "meta", for: "sortBy" }, plural(count, "show") + ", by "),
			el("select", {
				id: "sortBy", class: "select",
				onchange: e => { sort = e.target.value; paint(); },
			}, Object.entries(shows.SORTS).map(([key, spec]) =>
				el("option", { value: key, selected: sort === key }, spec.label))),
		]),
	]);
}

// pickVenue turns a listing back into a museum. The exhibition carries its
// venue's own coordinates, so the map can always be moved even when the venue
// cannot be matched to a catalogue record.
function pickVenue(show) {
	globe.reveal(show.longitude, show.latitude, 15);

	const key = show.museum_wikidata_id || normalise(show.museum);
	const match = museums.find(museum => keyFor(museum) === key);
	if (match) openMuseum?.(match.id);
}

// openVenue is the same thing from the other direction: a ring on the map
// rather than a row in the list. The ring carries only the venue's name, which
// is why it resolves against the listings first — those know the identifier.
export function openVenue({ name }) {
	const key = normalise(name);
	const show = listings.find(listing => normalise(listing.museum) === key);
	if (show) { pickVenue(show); return; }

	const match = museums.find(museum => normalise(museum.name) === key);
	if (match) openMuseum?.(match.id);
}

/* ---- reading the websites ----------------------------------------------- */

// looked records that a read of this area has just finished, and how it ended.
// Cleared when the area changes, because it is a fact about this place only.
let scraping = false, progressBox = null, looked = null;

export async function look() {
	if (!current || scraping) return;

	scraping = true;
	looked = null;
	progressBox = scrape.progress(null);
	paint();

	const started = await scrape.start(current.spot);
	if (!started.ok) {
		scraping = false;
		progressBox = el("div", { class: "coverage" }, [
			started.error,
			el("div", { class: "meta" }, "Nothing was read; you can try again."),
		]);
		paint();
		return;
	}

	scrape.announce(started.status);
	if (!api.running(started.status)) {
		// Nothing to wait for: either the area was read recently enough that the
		// server declined, or there was nothing here to read at all.
		finish(started.status.state);
		return;
	}

	progressBox = scrape.progress(started.status);
	paint();

	scrape.watch("area", current.spot, {
		onUpdate: status => {
			scrape.announce(status);
			progressBox = scrape.progress(status);
			paint();
		},
		onDone: status => finish(status ? status.state : "done"),
	});
}

async function finish(state = "done") {
	looked = state;
	scraping = false;
	progressBox = null;
	hud.setNote("");

	if (!current) return;
	const onShow = await api.exhibitionsNear(current.spot);
	if (!current || !onShow.ok) { paint(); return; }

	listings = onShow.data.exhibitions || [];
	report = onShow.data.coverage;
	globe.showVenues(listings);
	hud.say(listings.length
		? "Finished reading. " + plural(listings.length, "show") + " found."
		: "Finished reading. Nothing on show was found here.");
	tab = "shows";
	paint();
}

// listingsFor hands the museum card whatever has already been fetched for the
// area, so opening a museum inside a city that is already loaded costs no
// second request.
export function listingsFor(museum) {
	if (!current) return null;
	const key = keyFor(museum);
	return listings.filter(show =>
		(show.museum_wikidata_id || normalise(show.museum)) === key);
}

export function area() {
	return current;
}
