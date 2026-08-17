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
import { el, clear, plural, haversine } from "./util.js";

export const card = new Card({ modifier: "card--area", onClose: () => {
	globe.clearVenues();
	scrape.stop("area");
	inFlight?.abort();
	inFlight = null;
	loading = false;
	current = null;
} });

let current = null;   // { name, spot }
let museums = [];
let listings = [];
let report = null;
// What the listings are doing, as distinct from what they hold. An empty list
// is an answer — this city has nothing on — and it must not be what a request
// that failed, timed out, or has not come back yet looks like.
let loading = false, failure = null;
let inFlight = null;
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
	// A look started at the last place stops being this card's business the
	// moment the card is about somewhere else. Left running, its poll painted
	// one city's progress into another city's panel, and the flag it left
	// standing is what made "Look for exhibitions here" do nothing at the new
	// place: look() refuses to start while a scrape it believes is its own is
	// still going.
	scrape.stop("area");
	scraping = false;
	progressBox = null;
	// A different place has not been looked at, whatever happened at the last one.
	looked = null;

	current = { name: String(place.name || "").split(",")[0], spot };
	tab = "museums";

	// Superseded requests are cancelled rather than left to arrive and be
	// ignored. Two cities asked for in quick succession are two full pages of
	// listings still crossing the wire for a card that has moved on.
	inFlight?.abort();
	const controller = new AbortController();
	inFlight = controller;

	loading = true;
	failure = null;
	listings = [];
	report = null;
	card.setTitle(current.name).open().busy("Looking…");

	// Both at once: the museums here, and everything on show, so each museum
	// can say how much of it is theirs.
	const [nearby, onShow] = await Promise.all([
		api.museumsNear(spot, 200, controller.signal),
		api.exhibitionsNear(spot, controller.signal),
	]);
	if (controller !== inFlight || current?.spot !== spot) return; // a newer place is showing
	inFlight = null;
	loading = false;

	if (!nearby.ok) {
		if (nearby.aborted) return;
		card.failed(nearby.error || "Could not load this area.", () => show(place));
		return;
	}

	museums = nearby.data.museums || [];
	takeListings(onShow);

	hud.summarise(current.name, nearby.data.total);
	paint();
}

// takeListings records one answer about what is on, whatever kind of answer it
// was. The panel reads these three together, so they are only ever written
// together — an error left behind by the last attempt sitting above a list that
// has since loaded is its own kind of wrong.
function takeListings(result) {
	if (result.ok) {
		listings = result.data.exhibitions || [];
		report = result.data.coverage || null;
		failure = null;
	} else {
		listings = [];
		report = null;
		failure = result.error || "Could not load what is on here.";
	}
	globe.showVenues(listings);
}

// reload asks again for the listings alone. The museums in a city are a settled
// fact that rarely fails; what is on there is read live and is what a retry is
// actually for.
async function reload() {
	if (!current || loading) return;
	const spot = current.spot;

	inFlight?.abort();
	const controller = new AbortController();
	inFlight = controller;

	loading = true;
	failure = null;
	paint();

	const onShow = await api.exhibitionsNear(spot, controller.signal);
	if (controller !== inFlight || current?.spot !== spot) return;
	inFlight = null;
	loading = false;

	if (onShow.aborted) return;
	takeListings(onShow);
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

	// Three things a reader must be able to tell apart, and only one of them is
	// a list. This tab used to render nothing at all when the request failed —
	// same blank space as a city with nothing on, and no way to ask again.
	if (loading) {
		return el("div", { class: "empty", "data-glyph": "⋯" }, "Looking for what's on…");
	}
	if (failure) {
		return el("div", { class: "empty", "data-glyph": "!" }, [
			failure,
			el("div", {}, el("button", { class: "linkish", type: "button", onclick: reload },
				"Try again")),
		]);
	}

	const kept = listings.filter(shows.FILTERS[filter].keep);
	const summary = shows.coverage(report, listings, {
		scraping: scraping,
		looked: looked,
		onScrape: () => look(),
	});

	if (summary) out.append(summary);
	if (progressBox) out.append(progressBox);

	if (listings.length) {
		// Counted without the permanent ones, because they are not what the
		// count sits above: they are folded away below, under their own number.
		out.append(controls(kept.filter(show => !show.permanent).length));
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

		// The count and the ordering describe the list directly beneath them, so
		// with nothing time-limited on there is nothing for them to describe:
		// "86 shows, by closing soon" sitting on top of "nothing with an end
		// date is on here" contradicts itself, and the 86 it means are the
		// permanent displays, folded below under their own number. The periods
		// stay — switching them is how something upcoming is found.
		count > 0 && el("div", { class: "controls__sort" }, [
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

	// The place this look is about, held rather than read back from current: a
	// read takes minutes, and every step of it after this one has to be able to
	// tell whether the card is still about the same place.
	const spot = current.spot;

	scraping = true;
	looked = null;
	progressBox = scrape.progress(null);
	// Where the reading is happening is where it should be watched. A look
	// started from the pill left the card on its museums tab, so the bar, the
	// site counter and every word about what was going on were drawn into a tab
	// nobody was looking at: pressing "Look for exhibitions here" appeared to do
	// nothing for the minutes it took.
	tab = "shows";
	paint();

	const started = await scrape.start(spot);
	if (current?.spot !== spot) return;
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
		finish(spot, started.status.state);
		return;
	}

	progressBox = scrape.progress(started.status);
	paint();

	scrape.watch("area", spot, {
		onUpdate: status => {
			if (current?.spot !== spot) return;
			scrape.announce(status);
			progressBox = scrape.progress(status);
			paint();
		},
		onDone: status => finish(spot, status ? status.state : "done"),
	});
}

async function finish(spot, state = "done") {
	if (current?.spot !== spot) return;

	looked = state;
	hud.setNote("");

	// The read is over, but what it found is still one request away, and until
	// it lands this counts as still reading: the bar stays up, and nothing draws
	// a verdict from the listings fetched before the read began. "Nothing was
	// found here just now", printed over results that are on their way, is a
	// wrong answer with half a second to live — and it is the half second
	// somebody who just pressed the button is watching.
	progressBox = scrape.progress(null);
	tab = "shows";
	paint();

	inFlight?.abort();
	const controller = new AbortController();
	inFlight = controller;

	const onShow = await api.exhibitionsNear(spot, controller.signal);
	if (controller !== inFlight || current?.spot !== spot) return;
	inFlight = null;
	progressBox = null;
	scraping = false;

	if (onShow.aborted) { paint(); return; }
	takeListings(onShow);

	hud.say(failure ? "Finished reading, but the listings could not be loaded."
		: listings.length
			? "Finished reading. " + plural(listings.length, "show") + " found."
			: "Finished reading. Nothing on show was found here.");
	paint();
}

// listingsFor hands the museum card whatever has already been fetched for the
// area, so opening a museum inside a city that is already loaded costs no
// second request.
//
// Inside is the whole condition. A card stays open when the map moves on, so a
// museum clicked in Karlskrona used to be answered out of the listings fetched
// for Kalmar: nothing there is its, the filter came back empty, and the panel
// said "nothing listed for this museum" without ever asking. Null is the answer
// that sends the museum card to fetch for itself — which is also what a load
// that failed or has not landed yet must return, since the shortcut is only
// worth taking when there is a real answer to hand.
//
// The coverage report travels with the listings. Without it the museum card
// could not tell "this city has been read and your museum lists nothing" from
// "nobody has read this city yet", so opening a museum answered one way with
// the area card open and the other way without it.
export function listingsFor(museum) {
	if (!current || loading || failure) return null;
	if (!holds(museum.latitude, museum.longitude)) return null;

	const key = keyFor(museum);
	return {
		shows: listings.filter(show =>
			(show.museum_wikidata_id || normalise(show.museum)) === key),
		report,
	};
}

// covers says whether a view is still the area this card is about.
//
// The card is a question asked about one circle, and the map is free to leave
// it. Anything that acts on "here" has to ask this first, or it acts on there.
//
// Where, not how big. A view is several times wider than the place at its
// centre — searching a city leaves the map showing thirty kilometres around a
// ten-kilometre town — so comparing the two radii would call the card stale the
// moment it opened, and every look would throw away the name of the city that
// was searched for. What is left is bounded anyway: nothing offers to read an
// area wider than the API's own limit.
export function covers(spot) {
	return Boolean(current && spot) && holds(spot.lat, spot.lon);
}

// holds is "this point is in the card's circle".
function holds(lat, lon) {
	if (!current) return false;
	if (!Number.isFinite(lat) || !Number.isFinite(lon)) return false;
	// A quarter of the catalogue has no coordinates and serialises as 0,0, which
	// is a real position in the Gulf of Guinea and belongs to no city's card.
	if (!lat && !lon) return false;
	return haversine(current.spot.lat, current.spot.lon, lat, lon) <= current.spot.radiusKm;
}
