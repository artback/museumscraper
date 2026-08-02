// Small shared helpers: building DOM, talking to the API, and saying dates the
// way a visitor reads them.

/* ---- DOM ---------------------------------------------------------------- */

// el builds an element. The page used to assemble itself by concatenating HTML
// strings and escaping the values by hand at each site, which is a rule that
// has to be remembered every time — and the values here are museum names and
// exhibition titles read off other people's websites, so forgetting once is an
// injection. Setting text through the DOM cannot be forgotten.
//
//   el("div", { class: "row" }, [el("b", {}, museum.name), " · ", "1.2 km"])
//
// Strings and numbers among the children become text nodes; null and undefined
// are skipped, so a conditional child can be written inline.
export function el(tag, props = {}, children = []) {
	const node = document.createElement(tag);

	for (const [key, value] of Object.entries(props)) {
		if (value === null || value === undefined || value === false) continue;
		if (key === "class") node.className = value;
		else if (key === "text") node.textContent = value;
		else if (key === "dataset") Object.assign(node.dataset, value);
		else if (key.startsWith("on") && typeof value === "function") {
			node.addEventListener(key.slice(2).toLowerCase(), value);
		} else node.setAttribute(key, value === true ? "" : value);
	}

	for (const child of [].concat(children)) {
		if (child === null || child === undefined || child === false) continue;
		node.append(child instanceof Node ? child : String(child));
	}
	return node;
}

// clear empties a node and returns it, so a render function can start from a
// known state without reaching for innerHTML.
export function clear(node) {
	node.replaceChildren();
	return node;
}

// link is an external link, with the target checked.
//
// A museum's website and an exhibition's URL are scraped, so the scheme is not
// something this page gets to assume: a "javascript:" URL in an href runs when
// clicked. Anything that is not plain http(s) is rendered as text instead.
export function link(url, text, extra = {}) {
	const safe = safeURL(url);
	if (!safe) return el("span", { text });
	return el("a", { href: safe, target: "_blank", rel: "noopener noreferrer", ...extra }, text);
}

export function safeURL(url) {
	if (!url) return null;
	try {
		const parsed = new URL(url, window.location.origin);
		return parsed.protocol === "http:" || parsed.protocol === "https:" ? parsed.href : null;
	} catch (_) {
		return null;
	}
}

/* ---- the API ------------------------------------------------------------ */

// getJSON separates "the server said no" from "the server said nothing".
//
// The page used to return null for every failure, so a backend that was down
// and a city with no museums produced the same empty panel. They need different
// words on screen, so they need to be different answers here.
export async function getJSON(url, options = {}) {
	try {
		const res = await fetch(url, options);
		if (!res.ok) {
			return { ok: false, status: res.status, error: await errorText(res) };
		}
		return { ok: true, data: await res.json() };
	} catch (err) {
		// An aborted request is this page superseding itself, not a failure to
		// report: the caller that aborted it is already drawing something newer.
		if (err.name === "AbortError") return { ok: false, aborted: true };
		return { ok: false, error: "Could not reach the catalogue." };
	}
}

// errorText prefers what the server said. The API explains its refusals in
// sentences meant to be read — that a radius is too large, that too many areas
// are already queued — and the page used to discard all of them.
async function errorText(res) {
	try {
		const body = await res.json();
		if (body && typeof body.error === "string" && body.error) return sentence(body.error);
	} catch (_) { /* not JSON, fall through */ }
	return res.status >= 500 ? "The catalogue is having trouble." : "That request was refused.";
}

function sentence(text) {
	const trimmed = text.trim();
	if (!trimmed) return trimmed;
	return trimmed[0].toUpperCase() + trimmed.slice(1) + (/[.!?]$/.test(trimmed) ? "" : ".");
}

/* ---- geography ---------------------------------------------------------- */

export function haversine(lat1, lon1, lat2, lon2) {
	const R = 6371, r = Math.PI / 180;
	const dLat = (lat2 - lat1) * r, dLon = (lon2 - lon1) * r;
	const a = Math.sin(dLat / 2) ** 2 +
		Math.cos(lat1 * r) * Math.cos(lat2 * r) * Math.sin(dLon / 2) ** 2;
	return 2 * R * Math.asin(Math.min(1, Math.sqrt(a)));
}

// A radius in kilometres as a zoom level: each level halves the span, and
// level 9 is roughly a 40 km view at the equator.
export function zoomForRadius(km) {
	const z = 9 - Math.log2(Math.max(km, 1) / 20);
	return Math.max(3, Math.min(15, z));
}

/* ---- dates -------------------------------------------------------------- */

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun",
	"Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

function day(value) {
	const d = new Date(value);
	return Number.isNaN(d.getTime()) ? null : d;
}

export function shortDate(value) {
	const d = day(value);
	if (!d) return "";
	const now = new Date();
	const date = d.getUTCDate() + " " + MONTHS[d.getUTCMonth()];
	return d.getUTCFullYear() === now.getUTCFullYear() ? date : date + " " + d.getUTCFullYear();
}

export function daysUntil(value) {
	const d = day(value);
	if (!d) return null;
	const midnight = Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate());
	const now = new Date();
	const today = Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate());
	return Math.round((midnight - today) / 86400000);
}

// howLong turns a count of days into the phrase that decides whether someone
// goes this week — "3 days left" is an instruction, "closes 14 Sep" is a fact.
export function howLong(days) {
	if (days === null) return "";
	if (days < 0) return "closed";
	if (days === 0) return "last day";
	if (days === 1) return "1 day left";
	if (days < 14) return days + " days left";
	if (days < 60) return Math.round(days / 7) + " weeks left";
	return "";
}

// when describes an exhibition's dates the way a listing does.
//
// The three flags the API sends exist precisely so this does not have to be
// guessed from empty dates: a permanent display has none because it is always
// on, and a listing the scraper failed on has none because it failed.
export function when(show) {
	if (show.permanent) return { label: "Always on show", kind: "permanent" };

	if (show.upcoming && show.start) {
		return { label: "Opens " + shortDate(show.start), kind: "upcoming" };
	}
	if (show.end) {
		const left = howLong(daysUntil(show.end));
		return {
			label: "Closes " + shortDate(show.end) + (left ? " · " + left : ""),
			kind: daysUntil(show.end) <= 14 ? "closing" : "running",
		};
	}
	if (show.running) return { label: "On now", kind: "running" };
	return { label: "Dates not published", kind: "unknown" };
}

/* ---- motion ------------------------------------------------------------- */

export function prefersReducedMotion() {
	return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}

export function plural(n, one, many = one + "s") {
	return n.toLocaleString() + " " + (n === 1 ? one : many);
}
