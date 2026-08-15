// The right-hand column and the cards in it.
//
// Every panel used to be positioned at the same top-right corner and the
// collision was settled by a stylesheet rule that read the inline style
// attribute of a sibling. That rule matched before any museum had been opened —
// so the area list was hidden by default and only appeared because opening a
// museum happened to stamp `display:none` on the detail panel first — and it
// hid the list permanently once a museum had been opened, which is why closing
// a museum used to leave nowhere to go back to.
//
// A column that owns the edge, and cards that stack in it, is the same idea
// without the coupling: a card knows whether it is open, the column decides
// where it sits, and adding a fourth one is appending an element.

import { el, clear } from "./util.js";

let sequence = 0;

export class Card {
	// modifier is an optional extra class, so a card can be styled by what it
	// holds without the dock knowing anything about it.
	constructor({ title = "", modifier = "", onClose = null } = {}) {
		const id = "card-" + (++sequence);
		this.onClose = onClose;

		this.title = el("button", {
			class: "card__toggle",
			type: "button",
			"aria-expanded": "true",
			"aria-controls": id + "-body",
			onclick: () => this.toggle(),
		}, title);

		this.body = el("div", { class: "card__body", id: id + "-body" });

		this.closeButton = el("button", {
			class: "icon-btn",
			type: "button",
			"aria-label": "Close",
			onclick: () => this.close(),
		}, "×");

		this.root = el("section", { class: "card " + modifier, hidden: true }, [
			el("div", { class: "card__head" }, [
				// The button sits inside the heading rather than around it:
				// a heading is not phrasing content, so a button wrapping one
				// is invalid and assistive technology stops reporting the
				// level. This is the shape every accordion pattern uses.
				el("h2", { class: "card__title" }, this.title),
				this.closeButton,
			]),
			this.body,
		]);
	}

	setTitle(text) {
		clear(this.title).append(text);
		return this;
	}

	get isOpen() {
		return !this.root.hidden;
	}

	get expanded() {
		return this.title.getAttribute("aria-expanded") === "true";
	}

	// open shows the card and gives it the column's attention, folding the
	// others rather than hiding them: the list you arrived from stays one click
	// away instead of being destroyed.
	open() {
		this.root.hidden = false;
		this.expand();
		return this;
	}

	expand() {
		this.title.setAttribute("aria-expanded", "true");
		this.root.classList.add("card--focus");
		for (const other of siblings(this)) other.collapse();
		return this;
	}

	collapse() {
		this.title.setAttribute("aria-expanded", "false");
		this.root.classList.remove("card--focus");
		return this;
	}

	toggle() {
		return this.expanded ? this.collapse() : this.expand();
	}

	close() {
		if (!this.isOpen) return this;
		this.root.hidden = true;
		clear(this.body);
		if (this.onClose) this.onClose();
		// Whatever is still open takes the attention back, so closing the top
		// card reveals the one underneath rather than leaving a folded stack.
		const next = siblings(this).find(card => card.isOpen);
		if (next) next.expand();
		else restoreFocus();
		return this;
	}

	// busy paints the card while something is in flight. A panel that opens
	// empty and fills in later is indistinguishable from one that failed, and
	// the fetch behind these is sometimes a geocoder taking seconds.
	busy(message = "Loading…") {
		clear(this.body).append(el("div", { class: "empty", "data-glyph": "⋯" }, message));
		return this;
	}

	failed(message, retry = null) {
		clear(this.body).append(el("div", { class: "empty", "data-glyph": "!" }, [
			message,
			retry && el("div", {}, el("button", {
				class: "linkish", type: "button", onclick: retry,
			}, "Try again")),
		]));
		return this;
	}
}

const cards = [];
let dock = null;
let focusBeforePanel = null;

function siblings(card) {
	return cards.filter(other => other !== card);
}

// remember what had focus before a panel took it, so closing the panel puts it
// back rather than dropping focus onto the document.
export function rememberFocus() {
	const active = document.activeElement;
	if (active && active !== document.body) focusBeforePanel = active;
}

function restoreFocus() {
	if (focusBeforePanel && document.contains(focusBeforePanel)) {
		focusBeforePanel.focus();
	}
	focusBeforePanel = null;
}

// mount puts the cards in the column. Order is priority: the most specific
// thing — the museum you just opened — sits at the top.
export function mount(container, ...toAdd) {
	dock = container;
	for (const card of toAdd) {
		cards.push(card);
		dock.append(card.root);
	}
}

export function openCards() {
	return cards.filter(card => card.isOpen);
}

// closeTop is what Escape does: shut the innermost thing rather than everything
// at once, so Escape twice from a museum leaves the area list showing.
export function closeTop() {
	const open = cards.filter(card => card.isOpen);
	if (!open.length) return false;
	const focused = open.find(card => card.expanded) || open[0];
	focused.close();
	return true;
}
