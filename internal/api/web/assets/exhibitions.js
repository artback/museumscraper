// What is on show, as a list.
//
// Exhibitions used to appear only inside one museum's panel, found by asking
// for everything within a kilometre and keeping the rows whose venue name
// matched the museum's exactly. This module is the other half: everything on
// show in an area, which is the question somebody standing in a city actually
// has.

import { MAX_EXHIBITIONS } from "./api.js";
import { el, link, when, plural, shortDate } from "./util.js";

/* ---- which ones --------------------------------------------------------- */

// Weekend is the coming Saturday and Sunday, or this one if it is already the
// weekend — "this weekend" on a Sunday means today, not in six days.
function weekend() {
	const now = new Date();
	const day = now.getUTCDay();
	const toSaturday = day === 0 ? -1 : 6 - day;
	const from = new Date(now);
	from.setUTCDate(now.getUTCDate() + toSaturday);
	const to = new Date(from);
	to.setUTCDate(from.getUTCDate() + 1);
	return [from, to];
}

function overlaps(show, from, to) {
	const start = show.start ? new Date(show.start) : null;
	const end = show.end ? new Date(show.end) : null;
	if (start && start > to) return false;
	if (end && end < from) return false;
	return true;
}

export const FILTERS = {
	now: {
		label: "On now",
		keep: show => show.running || show.permanent,
	},
	weekend: {
		label: "This weekend",
		keep: show => {
			if (show.permanent) return true;
			const [from, to] = weekend();
			return overlaps(show, from, to);
		},
	},
	month: {
		label: "Next 30 days",
		keep: show => {
			if (show.permanent) return true;
			const from = new Date();
			const to = new Date();
			to.setUTCDate(to.getUTCDate() + 30);
			return overlaps(show, from, to);
		},
	},
};

export const SORTS = {
	closing: {
		label: "Closing soon",
		// Undated and permanent shows have no deadline, so they sort last
		// rather than pretending to be urgent.
		compare: (a, b) => rank(a) - rank(b) || a.distance_km - b.distance_km,
	},
	nearest: {
		label: "Nearest",
		compare: (a, b) => a.distance_km - b.distance_km,
	},
	venue: {
		label: "By museum",
		compare: (a, b) => a.distance_km - b.distance_km ||
			String(a.museum).localeCompare(String(b.museum)),
	},
};

function rank(show) {
	if (show.permanent || !show.end) return Number.MAX_SAFE_INTEGER;
	const end = new Date(show.end).getTime();
	return Number.isNaN(end) ? Number.MAX_SAFE_INTEGER : end;
}

/* ---- the list ----------------------------------------------------------- */

// render draws the shows. Permanent displays are split out rather than mixed
// in: the scraper emits the museum itself as a permanent row when it can find
// no programme, so a plain list of a small city reads as a dozen rows of
// "Kalmar konstmuseum — Kalmar konstmuseum" with the real exhibitions beneath.
export function render(shows, { sort = "closing", onPick } = {}) {
	const temporary = shows.filter(show => !show.permanent);
	const permanent = shows.filter(show => show.permanent);
	const out = [];

	if (temporary.length) {
		out.push(section(temporary, sort, onPick));
	} else if (permanent.length) {
		// Somewhere with nothing time-limited on is a real and common answer —
		// most of a small city, most of the year. Said plainly, because the
		// alternative is what this looked like before: a sort control, a count,
		// and then a gap where the list should be, which reads as a panel that
		// failed to load rather than a city with nothing closing.
		out.push(el("div", { class: "empty", "data-glyph": "◎" }, [
			"Nothing with an end date is on here",
			el("small", {}, "The permanent collections below are open as usual."),
		]));
	}

	if (permanent.length) {
		out.push(el("details", { class: "fold" }, [
			el("summary", {}, plural(permanent.length, "permanent display")),
			section(permanent, sort, onPick),
		]));
	}

	if (!out.length) return null;
	return el("div", { class: "shows" }, out);
}

function section(shows, sort, onPick) {
	const ordered = [...shows].sort(SORTS[sort]?.compare || SORTS.closing.compare);

	if (sort !== "venue") {
		return el("ul", { class: "shows__list" }, ordered.map(show => row(show, onPick, true)));
	}

	// Grouped by venue, in distance order, so a museum with four shows reads as
	// one place worth the trip rather than four scattered rows.
	const groups = new Map();
	for (const show of ordered) {
		const key = show.museum_wikidata_id || show.museum || "?";
		if (!groups.has(key)) groups.set(key, []);
		groups.get(key).push(show);
	}

	return el("div", {}, [...groups.values()].map(group => el("div", { class: "venue" }, [
		el("button", {
			class: "venue__head", type: "button",
			onclick: () => onPick?.(group[0]),
		}, [
			el("span", { class: "venue__name", text: group[0].museum || "Unnamed venue" }),
			el("span", { class: "meta", text: distance(group[0]) + " · " + plural(group.length, "show") }),
		]),
		el("ul", { class: "shows__list" }, group.map(show => row(show, onPick, false))),
	])));
}

function distance(show) {
	return Number.isFinite(show.distance_km) ? show.distance_km.toFixed(1) + " km" : "";
}

function row(show, onPick, withVenue) {
	const dates = when(show);

	return el("li", { class: "show show--" + dates.kind }, [
		// The title links to the exhibition's own page when it has one, and is
		// plain text when the scraped URL is not something safe to follow.
		el("div", { class: "show__title" }, link(show.url, show.title || "Untitled")),

		withVenue && el("button", {
			class: "show__venue linkish", type: "button",
			onclick: () => onPick?.(show),
		}, [show.museum || "Unnamed venue", el("span", { class: "meta" }, " · " + distance(show))]),

		el("div", { class: "show__when" }, dates.label),
	]);
}

/* ---- how much of this there is ------------------------------------------ */

// coverage says what the list is and is not.
//
// These listings are an opportunistic read of whatever websites happen to
// exist, so most areas hold little and some hold nothing. Saying so plainly is
// the difference between a thin list and a list that looks broken — and the
// numbers to say it with are in every response.
export function coverage(report, shows, { onScrape, scraping, looked } = {}) {
	if (!report) return null;

	const venues = new Set(shows.map(show => show.museum).filter(Boolean)).size;
	const withSite = report.museums_with_website || 0;
	const inArea = report.museums_in_area || 0;

	if (shows.length) {
		// A full page is the only signal that there were more: the API reports
		// no total for exhibitions. "500 shows at 40 venues" for a city holding
		// eight hundred is a number somebody plans a weekend around.
		const capped = shows.length >= MAX_EXHIBITIONS;
		return el("div", { class: "coverage" }, [
			(capped ? "The first " : "") + plural(shows.length, "show") +
				" at " + plural(venues, "venue"),
			el("span", { class: "meta" },
				" · read from " + withSite + " of " + plural(inArea, "museum") + " with a website" +
				(report.last_scraped ? " · checked " + shortDate(report.last_scraped) : "")),
		]);
	}

	// Nothing to list is four different situations, and they want four
	// different things from the reader. Saying "not scraped" for all of them
	// made an area with no websites to read look like one nobody had tried.
	if (inArea === 0) {
		return note("No museums are known here.", "Try a wider view.");
	}
	if (withSite === 0) {
		return note(
			"None of the " + plural(inArea, "museum") + " here publishes a website.",
			"There is nothing to read listings from.");
	}
	if (scraping) {
		return null; // the progress bar is already saying it
	}

	// A look that has just been and gone says so, whatever the coverage report
	// makes of it.
	//
	// The report is built from the sites that were actually visited, so an area
	// where there was nothing to visit comes back exactly as it went in — and
	// the panel used to answer a press of "Look for exhibitions here" by
	// offering it again, word for word, which reads as a button that does
	// nothing. It is now the one thing on screen that knows a look happened.
	if (looked) {
		return note(
			looked === "recently-scraped"
				? "These websites were read recently, and nothing is on show."
				: "Nothing was found here just now.",
			withSite > 0
				? "Museums here publish " + plural(withSite, "website") +
					", but none of them lists anything on at the moment."
				: "No museum here publishes a website that can be read.");
	}

	if (report.last_scraped) {
		return note(
			"Nothing is on show here.",
			"The websites were last read " + shortDate(report.last_scraped) + ".");
	}
	return el("div", { class: "coverage coverage--offer" }, [
		el("div", {}, "Nobody has read the websites here yet."),
		el("div", { class: "meta" }, plural(withSite, "museum") + " here has a website to read."),
		onScrape && el("button", { class: "button", type: "button", onclick: onScrape },
			"Look for exhibitions here"),
	]);
}

function note(headline, detail) {
	return el("div", { class: "coverage" }, [
		el("div", {}, headline),
		detail && el("div", { class: "meta" }, detail),
	]);
}
