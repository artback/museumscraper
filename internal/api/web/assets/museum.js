// One museum: what it is, and what is on there.

import * as api from "./api.js";
import * as globe from "./globe.js";
import * as scrape from "./scrape.js";
import * as shows from "./exhibitions.js";
import * as area from "./area.js";
import { Card, rememberFocus } from "./dock.js";
import { el, clear, link, shortDate } from "./util.js";

export const card = new Card({ modifier: "card--museum", onClose: () => {
	globe.select(null);
	scrape.stop("museum");
	open = null;
	onChange?.(null);
} });

let open = null, onChange = null;

export function onOpenChange(handler) {
	onChange = handler;
}

export function current() {
	return open;
}

// hasPosition is the difference between a museum on the map and one only search
// can reach. About a quarter of the catalogue has no coordinates, and those
// records serialise as latitude 0, longitude 0 — so asking what is on show near
// one used to send the panel to look at the Gulf of Guinea, and report back
// that nobody had scraped the Atlantic yet.
function hasPosition(museum) {
	return Number.isFinite(museum.latitude) && Boolean(museum.latitude || museum.longitude);
}

export async function show(id) {
	rememberFocus();
	card.setTitle("…").open().busy();

	const result = await api.museum(id);
	if (!result.ok) {
		card.failed(result.error || "Could not load this museum.", () => show(id));
		return;
	}

	const museum = result.data;
	open = museum;
	card.setTitle(museum.name);
	paint(museum);
	onChange?.(museum);

	if (hasPosition(museum)) {
		globe.select(museum.id);
		globe.reveal(museum.longitude, museum.latitude);
	} else {
		globe.select(null);
	}

	loadShows(museum);
}

function paint(museum) {
	const where = [museum.locality, museum.country].filter(Boolean).join(", ");
	const body = clear(card.body);

	if (where) body.append(el("div", { class: "meta where" }, where));

	const tags = el("div", { class: "tags" }, [
		museum.verified
			? el("span", { class: "tag tag--ok" }, "Wikipedia article")
			// The catalogue's unverified tail holds things that are not museums
			// at all. Saying nothing made a doubtful record look as settled as
			// a checked one.
			: el("span", { class: "tag" }, "Unconfirmed entry"),
		museum.approximate_location &&
			el("span", { class: "tag tag--warn" }, "Position approximate — town centre"),
		!hasPosition(museum) && el("span", { class: "tag tag--warn" }, "Position unknown"),
	]);
	if (tags.children.length) body.append(tags);

	if (museum.description) body.append(el("p", { class: "desc" }, museum.description));

	const links = [
		museum.website && link(museum.website, "Website", { class: "linkish" }),
		museum.wikipedia_url && link(museum.wikipedia_url, "Wikipedia", { class: "linkish" }),
	].filter(Boolean);
	if (links.length) body.append(el("div", { class: "links" }, links));

	body.append(el("section", { class: "block", id: "museumShows" }, [
		el("h3", {}, "What's on"),
		el("div", { class: "meta" }, "Looking…"),
	]));
}

function showsBox() {
	return document.getElementById("museumShows");
}

async function loadShows(museum) {
	const box = showsBox();
	if (!box) return;

	if (!hasPosition(museum)) {
		paintShows(box, [], null, museum);
		return;
	}

	// Whatever the area card already fetched, if this museum is inside the
	// place being shown. Opening five museums in a city used to mean five more
	// requests for listings that were already in hand.
	const known = area.listingsFor(museum);
	if (known) {
		paintShows(box, known, null, museum);
		return;
	}

	const spot = { lat: museum.latitude, lon: museum.longitude, radiusKm: 1 };
	const result = await api.exhibitionsNear(spot);
	if (open !== museum) return;

	if (!result.ok) {
		clear(box).append(el("h3", {}, "What's on"),
			el("div", { class: "meta" }, result.error || "Could not load."));
		return;
	}

	const mine = (result.data.exhibitions || []).filter(show => belongsTo(show, museum));
	paintShows(box, mine, result.data.coverage, museum);
}

// belongsTo decides whether a listing is this museum's. Exhibitions are stored
// per venue and the radius is only how they are found, so proximity alone is
// not the answer.
function belongsTo(show, museum) {
	if (show.museum_wikidata_id && museum.wikidata_id) {
		return show.museum_wikidata_id === museum.wikidata_id;
	}
	const name = s => String(s ?? "").normalize("NFC").trim().toLowerCase().replace(/^the\s+/, "");
	return name(show.museum) === name(museum.name);
}

function paintShows(box, mine, coverage, museum) {
	clear(box).append(el("h3", {}, "What's on"));

	if (mine.length) {
		const checked = mine.map(show => show.scraped_at).filter(Boolean).sort().pop();
		if (checked) {
			// An eight-month-old listing used to look exactly as current as
			// one read this morning.
			box.append(el("div", { class: "meta" }, "Read from their website " + shortDate(checked)));
		}
		box.append(shows.render(mine, { sort: "closing" }));
		return;
	}

	if (!hasPosition(museum)) {
		box.append(el("div", { class: "meta" },
			"This museum has no position on file, so its listings cannot be looked up."));
		return;
	}

	box.append(el("div", { class: "meta" }, "Nothing listed for this museum."));

	// Offer to go and look, rather than reporting a state and stopping. The
	// museum's own area may simply never have been read.
	if (!coverage || coverage.last_scraped) return;
	box.append(el("button", {
		class: "button", type: "button",
		onclick: () => look(museum),
	}, "Read this museum's website"));
}

async function look(museum) {
	const box = showsBox();
	if (!box || !hasPosition(museum)) return;

	const spot = { lat: museum.latitude, lon: museum.longitude, radiusKm: 5 };
	clear(box).append(el("h3", {}, "What's on"), scrape.progress(null));

	const started = await scrape.start(spot);
	if (open !== museum) return;

	if (!started.ok) {
		clear(box).append(el("h3", {}, "What's on"), el("div", { class: "meta" }, started.error));
		return;
	}

	scrape.announce(started.status);
	if (!api.running(started.status)) {
		loadShows(museum);
		return;
	}

	clear(box).append(el("h3", {}, "What's on"), scrape.progress(started.status));

	scrape.watch("museum", spot, {
		onUpdate: status => {
			if (open !== museum) return;
			scrape.announce(status);
			const live = showsBox();
			if (live) clear(live).append(el("h3", {}, "What's on"), scrape.progress(status));
		},
		onDone: () => {
			scrape.announce(null);
			if (open === museum) loadShows(museum);
		},
	});
}
