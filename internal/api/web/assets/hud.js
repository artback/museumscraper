// The status line in the corner, and the channel that says the same things to
// a screen reader.
//
// These are deliberately two things. The visible line is rewritten on every
// settled pan, so marking it aria-live would narrate "38,412 museums shown"
// over and over as somebody moves the map, and would read out a changing digit
// every four seconds for the whole of a scrape. What is worth announcing is the
// change of state, not the change of number.

import { el, clear, plural } from "./util.js";

const count = document.getElementById("hudCount");
const note = document.getElementById("hudNote");
const live = document.getElementById("announce");

let lastSpoken = "";

// say announces, but only something genuinely new. Repeated identical messages
// are dropped, which is what stops a four-second poll becoming a metronome.
export function say(message) {
	if (!message || message === lastSpoken) return;
	lastSpoken = message;
	live.textContent = message;
}

export function museums(total, truncated) {
	clear(count).append(
		el("b", { text: total.toLocaleString() }),
		truncated ? " museums — the most notable here; zoom in for the rest" : " museums shown",
	);
}

// failed replaces the count with something that says so and offers the retry.
// A silent catch here used to leave a black globe and an empty corner, which is
// indistinguishable from a catalogue that is simply empty.
export function failed(message, retry) {
	clear(count).append(
		el("span", { class: "hud__bad", text: message || "Could not reach the catalogue." }),
		retry && el("button", { class: "linkish", type: "button", onclick: retry }, "Retry"),
	);
	say(message || "Could not reach the catalogue.");
}

export function loading() {
	clear(count).append("Loading museums…");
}

// setNote writes the second half of the line — what the scraper is doing.
// Kept as its own element because the count used to be written with innerHTML
// on the shared parent, which detached this one: the progress line vanished on
// every pan and only came back on the next poll, up to four seconds later.
export function setNote(text) {
	note.textContent = text ? "  ·  " + text : "";
}

export function summarise(place, total) {
	say("Showing " + plural(total, "museum") + " near " + place + ".");
}
