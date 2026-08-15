// Asking the server to read the museums' own websites for whatever is on show
// in an area, and reporting how that is going.
//
// This is bounded on the server — one area at a time, a cap per area, a day's
// cooldown — because it turns looking at a map into traffic on other people's
// sites. It used to be bounded on this side too, badly: the client asked for a
// 60 km circle when the API had never accepted more than 50, and recorded the
// area as asked-about *before* sending, so the 400 that came back marked the
// city permanently done for the session. Zooming into a city the ordinary way
// passed through that band, and the feature then stayed off however far you
// zoomed, with nothing on screen to say why.

import * as api from "./api.js";
import { el, plural } from "./util.js";
import * as hud from "./hud.js";

// CELL is the grid the server dedupes on (scrapeCellDegrees in scrape.go).
// Matching it matters: keyed finer, panning inside one server cell sent half a
// dozen POSTs that the server merged into the one job anyway.
const CELL = 0.25;

// A poll every four seconds, backing off, and an end to it. The old loop had no
// cap and no cancellation, so closing the panel left a tab asking every four
// seconds until the job's own fifteen-minute timeout.
const FIRST_DELAY = 4000, MAX_DELAY = 20000, MAX_POLLS = 90;

const asked = new Map();   // cell -> when it was asked about
const watchers = new Map(); // owner -> { timer, cancelled }

function cellOf({ lat, lon }) {
	return Math.round(lat / CELL) + "," + Math.round(lon / CELL);
}

// worthScraping is the same judgement the server makes: a place, not a
// continent. Reading "Europe" is neither useful nor polite.
export function worthScraping(spot, zoom) {
	return Number.isFinite(spot.radiusKm) && spot.radiusKm <= api.MAX_RADIUS_KM && zoom >= 8;
}

export function alreadyAsked(spot) {
	return asked.has(cellOf(spot));
}

// start asks for an area to be read.
//
// The area is recorded only once the server has accepted it, so a refusal — a
// full queue, a bad radius, a network blip — leaves the area askable again.
export async function start(spot) {
	const result = await api.startScrape(spot);
	if (!result.ok) {
		// The API explains its refusals in sentences meant to be read. All of
		// them used to be dropped on the floor.
		return { ok: false, error: result.error };
	}

	asked.set(cellOf(spot), Date.now());
	return { ok: true, status: result.data };
}

export async function statusAt(spot) {
	const result = await api.scrapeStatus(spot);
	return result.ok ? result.data : null;
}

// watch polls until the area is done, then tells the caller so it can reload
// whatever it is showing.
//
// Watchers are keyed by owner because there are two of them — the map view and
// the open museum — and they watch different places. With a single shared timer
// they cancelled each other at random, so an area could stop being polled while
// it was still running, and the progress of one area could be painted into a
// panel about another.
export function watch(owner, spot, { onUpdate, onDone }) {
	stop(owner);
	const watcher = { cancelled: false, polls: 0, delay: FIRST_DELAY };
	watchers.set(owner, watcher);

	const poll = async () => {
		if (watcher.cancelled) return;

		const status = await statusAt(spot);
		if (watcher.cancelled) return;

		if (api.running(status)) {
			onUpdate?.(status);
			if (++watcher.polls >= MAX_POLLS) {
				stop(owner);
				onDone?.(null);
				return;
			}
			watcher.delay = Math.min(watcher.delay * 1.15, MAX_DELAY);
			watcher.timer = setTimeout(poll, watcher.delay);
			return;
		}

		stop(owner);
		onDone?.(status);
	};

	watcher.timer = setTimeout(poll, watcher.delay);
}

export function stop(owner) {
	const watcher = watchers.get(owner);
	if (!watcher) return;
	watcher.cancelled = true;
	clearTimeout(watcher.timer);
	watchers.delete(owner);
}

/* ---- saying what is happening -------------------------------------------- */

// describe says what is happening in one line. Sites read out of sites known is
// the only honest measure of how much is left, and the count found so far is
// the only one that says whether it is worth waiting for.
export function describe(status) {
	if (!status) return "";

	if (status.state === "queued") {
		// Not "first": several areas are read at once, so a queued one waits
		// alongside the others rather than behind them.
		const also = status.progress?.waiting_behind || 0;
		return also
			? "Queued — " + plural(also, "other area") + " waiting too"
			: "Queued — starting shortly";
	}
	if (status.state !== "running") return "";

	const p = status.progress || {};
	if (!p.sites) return "Finding museum websites here…";
	return "Reading museum websites — " + (p.sites_read || 0) + " of " + p.sites +
		(p.exhibitions_found ? " · " + plural(p.exhibitions_found, "listing") + " so far" : "");
}

// progress renders the same thing as a block, for a panel rather than the
// status line. Reading a city's websites takes minutes, and a bar that moves is
// the difference between "working" and "broken" to someone looking at it.
export function progress(status) {
	const p = (status && status.progress) || {};
	const known = status && status.state === "running" && p.sites > 0;
	const pct = known ? Math.round(100 * (p.sites_read || 0) / p.sites) : 0;

	const bar = el("div", {
		class: "progress" + (known ? "" : " waiting"),
		role: "progressbar",
		"aria-valuemin": "0",
		"aria-valuemax": "100",
		// An indeterminate bar is one with no value, which is what says to a
		// screen reader that the end is not known rather than that it is at zero.
		...(known ? { "aria-valuenow": String(pct) } : {}),
		"aria-valuetext": describe(status) || "Working",
	}, el("i", { style: "width:" + (known ? pct : 30) + "%" }));

	return el("div", { class: "scrape" }, [bar, el("div", { class: "meta", text: describe(status) })]);
}

// announce narrates a scrape to the status line and, sparingly, to a screen
// reader: the transitions, not every four-second tick of a counter.
export function announce(status) {
	hud.setNote(describe(status));
	if (!status) return;
	if (status.state === "queued") hud.say("Queued to read museum websites here.");
	else if (status.state === "running") hud.say("Reading museum websites here.");
}
