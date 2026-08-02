// The search box.
//
// Places and museums together, because "Gothenburg" and "Röhsska Museum" are
// the same question asked at different scales, and a box that only knows one of
// them sends you away to find the other.

import * as api from "./api.js";
import { el, clear, plural } from "./util.js";

const box = document.getElementById("q");
const results = document.getElementById("results");

// Two debounces, not one. /v1/search is a local index and can afford a keypress
// pause; /v1/places resolves through Nominatim, whose usage policy is not a
// suggestion, and typing "Gothenburg" at speed used to send a geocode request
// for most of its prefixes.
const MUSEUM_DELAY = 180, PLACE_DELAY = 450;

let museumTimer = null, placeTimer = null;
let inFlight = null;
let hits = [], cursor = -1;
let onPlace = null, onMuseum = null;

export function wire(handlers) {
	onPlace = handlers.onPlace;
	onMuseum = handlers.onMuseum;

	box.addEventListener("input", () => {
		const term = box.value.trim();
		clearTimeout(museumTimer);
		clearTimeout(placeTimer);

		if (!term) { dismiss(); return; }

		// Drawn before either request settles. The dropdown used to keep the
		// previous term's results on screen — and clickable — for as long as
		// the slowest of the two took, so a pause mid-word could fly the map
		// somewhere nobody had asked for.
		hits = [];
		cursor = -1;
		draw([el("li", { class: "hit hit--quiet" }, "Searching…")]);

		museumTimer = setTimeout(() => run(term, false), MUSEUM_DELAY);
		placeTimer = setTimeout(() => run(term, true), PLACE_DELAY);
	});

	box.addEventListener("keydown", onKey);

	// A dropdown with no way to dismiss it sat over the map indefinitely.
	document.addEventListener("pointerdown", e => {
		if (!document.getElementById("search").contains(e.target)) dismiss();
	});
}

export function focus() {
	box.focus();
	box.select();
}

function dismiss() {
	clearTimeout(museumTimer);
	clearTimeout(placeTimer);
	inFlight?.abort();
	inFlight = null;
	hits = [];
	cursor = -1;
	clear(results);
	box.setAttribute("aria-expanded", "false");
}

/* ---- asking ------------------------------------------------------------- */

let generation = 0;
let found = { museums: [], places: [] };

async function run(term, includePlaces) {
	const mine = ++generation;
	inFlight?.abort();
	const controller = new AbortController();
	inFlight = controller;

	const [museums, places] = await Promise.all([
		api.search(term, 10, controller.signal),
		includePlaces ? api.places(term, controller.signal) : Promise.resolve(null),
	]);
	if (mine !== generation) return;

	if (museums?.ok) found.museums = museums.data.museums || [];
	if (places?.ok) found.places = places.data.places || [];
	else if (places && !places.ok && !places.aborted) found.places = [];

	// Places lead: somebody typing a city name wants to go there, and the
	// museums in it are what they will see when they arrive.
	hits = [
		...found.places.map(place => ({ kind: "place", ...place })),
		...found.museums.map(museum => ({ kind: "museum", ...museum })),
	];
	cursor = -1;

	if (!hits.length) {
		const failed = museums && !museums.ok && !museums.aborted;
		draw([el("li", { class: "hit hit--quiet" }, [
			failed ? (museums.error || "Search is unavailable.") : "Nothing found",
			!failed && el("small", {}, "Try a city, or part of a museum's name."),
		])]);
		return;
	}

	draw(hits.map((hit, i) => option(hit, i)));
}

function option(hit, i) {
	const label = hit.kind === "place"
		? [el("b", {}, "◎ " + hit.name),
			el("small", {}, "place · " + Math.round(hit.radius_km) + " km around")]
		: [el("b", {}, hit.name),
			el("small", {}, [hit.locality, hit.country].filter(Boolean).join(", ") +
				(hit.locatable ? "" : " · position unknown"))];

	return el("li", {
		class: "hit",
		id: "hit-" + i,
		role: "option",
		"aria-selected": "false",
		onclick: () => choose(hit),
	}, label);
}

function draw(children) {
	clear(results).append(...children);
	box.setAttribute("aria-expanded", "true");
	box.removeAttribute("aria-activedescendant");
}

/* ---- keyboard ----------------------------------------------------------- */

function onKey(e) {
	if (e.key === "Escape") { dismiss(); box.blur(); return; }
	if (!hits.length) return;

	if (e.key === "ArrowDown" || e.key === "ArrowUp") {
		e.preventDefault();
		cursor = (cursor + (e.key === "ArrowDown" ? 1 : hits.length - 1)) % hits.length;
		moveCursor();
	} else if (e.key === "Enter") {
		e.preventDefault();
		choose(hits[Math.max(cursor, 0)]);
	}
}

// The cursor used to be a background colour and nothing else, so where Enter
// would go was invisible to a screen reader and, at 1.2:1 against the row,
// nearly invisible to everyone else.
function moveCursor() {
	for (const [i, node] of [...results.children].entries()) {
		const on = i === cursor;
		node.classList.toggle("hit--on", on);
		node.setAttribute("aria-selected", String(on));
	}
	const active = results.children[cursor];
	if (!active) return;
	box.setAttribute("aria-activedescendant", active.id);
	active.scrollIntoView({ block: "nearest" });
}

// A place flies the map there and lists what is in it; a museum opens its card.
function choose(hit) {
	if (!hit) return;
	dismiss();
	if (hit.kind === "place") onPlace?.(hit);
	else onMuseum?.(hit.id);
}
