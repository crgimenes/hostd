// What is left for the window to do. Everything that comes from the fleet is
// rendered where the fleet is and arrives as HTML, so this is only the part
// that is about the pointer and the clock: dragging, selecting text, and the
// two labels that would otherwise make the machine send a fragment every
// second for something it did not change.

const el = (id) => document.getElementById(id);

const MAX_LINES = 4000;

// hostdApply is how the fleet reaches this window: the Go side pushes the
// fragments that changed, and a round where nothing changed pushes nothing.
// The page never asks — there is no timer here, because a screen that polls
// touches its own HTML on a clock, and whatever the operator held is lost to
// the tick. The answer to a click arrives through the same door, so a push
// and an action cannot disagree about how the screen is written.
function hostdApply(html) {
	if (!html) {
		return;
	}
	// Read BEFORE anything is appended: once a line is in, the sentinel has
	// already moved and the question "was the reader at the end" can no longer
	// be asked.
	const following = atTail();
	const parsed = document.createElement("template");
	parsed.innerHTML = html;
	// Frozen first: replacing a piece pulls it out of the template, and a live
	// collection that shrinks mid-walk skips every other fragment.
	for (const piece of [...parsed.content.children]) {
		const swap = piece.getAttribute("data-swap") || "";
		if (swap.startsWith("append:")) {
			const target = document.querySelector(swap.slice("append:".length));
			if (target) {
				target.append(...piece.children);
			}
			continue;
		}
		// The whole list arriving means the log is now ABOUT somewhere else, and
		// a terminal opens at the end of its stream, not in the middle of it.
		if (piece.id === "lines") {
			movedLog = true;
		}
		const standing = document.getElementById(piece.id);
		if (standing) {
			standing.replaceWith(piece);
		}
	}
	settle(following);
}

// act carries one operator action to the panel over the glaze binding — the
// same kind of channel the pushes ride, so nothing on the way can lose its
// parameters. Outside the window (the test harness, a browser) the binding
// does not exist and the same action travels over HTTP instead.
function act(action) {
	if (window.hostd_act) {
		window.hostd_act(action).then(hostdApply).catch((err) => say("action failed: " + err, true));
		return;
	}
	fetch("/act/" + action)
		.then((response) => response.text())
		.then(hostdApply)
		.catch((err) => say(String(err), true));
}

// A failure in this script is written where the operator reads, not to a
// console nobody has open: a window that silently does nothing on click is a
// window nobody can report a defect about.
function say(text, bad) {
	const status = el("status");
	if (status) {
		status.textContent = text;
		status.classList.toggle("bad", Boolean(bad));
	}
}

window.onerror = (message) => {
	say(String(message), true);
};

// One listener for every act the page can ask: rows, twists, buttons that
// exist now and rows that arrive later all land here, so nothing needs wiring
// when a fragment is swapped in.
document.body.addEventListener("click", (event) => {
	// Chrome the page works by itself. Delegated like everything else: an
	// element inside a fragment is replaced by the next push, and a handler
	// bound straight to it is lost with the element it was bound to.
	const chrome = event.target.closest("[data-ui]");
	if (chrome) {
		if (chrome.dataset.ui === "nav") {
			toggleNav();
		}
		return;
	}
	const asked = event.target.closest("[data-act]");
	if (!asked) {
		return;
	}
	act(asked.dataset.act);
	// On a narrow window the sidebar covers what was just chosen: picking
	// something is done with it.
	if (NARROW.matches && asked.closest("#sidebar") && !asked.classList.contains("twist")) {
		awayNarrow = true;
		showNav();
	}
});

function settle(following) {
	// The log belongs to the machines: a page about the panel itself puts it
	// away rather than showing a pane about somewhere else.
	document.body.classList.toggle("noLog", el("detail").dataset.log === "off");
	const lines = el("lines");
	while (lines.children.length > MAX_LINES) {
		lines.firstElementChild.remove();
	}
	applySearch(movedLog);
	// A whole new list means the log is now ABOUT somewhere else: a terminal
	// opens at the end of its stream, and where the reader was in the previous
	// one means nothing here.
	if (movedLog) {
		following = true;
		movedLog = false;
	}
	if (following) {
		const box = el("lineBox");
		box.scrollTop = box.scrollHeight;
	}
	tick();
	armPlots();
}



/* Uptime is the one number that changes without anything happening. Counting
   it here keeps the machine from sending a fragment every second to say that a
   second passed. */

function tick() {
	for (const cell of document.querySelectorAll("[data-since]")) {
		const text = uptime(Number(cell.dataset.since));
		// Writing the same text still replaces the text node, which drops
		// whatever the operator had selected inside it.
		if (cell.textContent !== text) {
			cell.textContent = text;
		}
	}
}

function uptime(sinceMS) {
	if (!sinceMS) {
		return "-";
	}
	let seconds = Math.floor((Date.now() - sinceMS) / 1000);
	if (seconds < 0) {
		return "-";
	}
	const days = Math.floor(seconds / 86400);
	seconds -= days * 86400;
	const hours = Math.floor(seconds / 3600);
	seconds -= hours * 3600;
	const minutes = Math.floor(seconds / 60);
	seconds -= minutes * 60;
	if (days) {
		return `${days}d ${hours}h`;
	}
	if (hours) {
		return `${hours}h ${minutes}m`;
	}
	if (minutes) {
		return `${minutes}m ${seconds}s`;
	}
	return `${seconds}s`;
}

setInterval(tick, 1000);

/* Command+F, not a filter. What is found is marked where it stands and the
   lines around it stay: whoever searches a log almost always wants the line
   BEFORE the hit as much as the hit itself, and a pane that hid the rest would
   throw away the context that made the search worth doing.

   Everything here works on the lines already in the window. They are here; a
   round trip to ask the machine again would be a round trip to be told what
   the window already holds. */

let hits = [];
let hitAt = -1;

/* Marks one line, and leaves it completely alone when it already carries the
   marks for this exact term. That is not an optimisation: rewriting a line is
   what drops the text somebody is in the middle of selecting, which is the one
   thing this pane exists to preserve. */
function markLine(line, term) {
	if (line.dataset.marked === term) {
		return;
	}
	const cell = line.querySelector(".text");
	if (!cell) {
		return;
	}
	const raw = cell.dataset.raw ?? cell.textContent;
	cell.dataset.raw = raw;
	line.dataset.marked = term;
	cell.textContent = raw;
	if (!term) {
		return;
	}
	/* Built out of text nodes and elements, never out of a string of HTML: a
	   log line is whatever some program decided to write, and putting that
	   through innerHTML would let a service's output write this window. */
	cell.textContent = "";
	const needle = term.toLowerCase();
	const hay = raw.toLowerCase();
	let at = 0;
	for (;;) {
		const found = hay.indexOf(needle, at);
		if (found < 0) {
			break;
		}
		if (found > at) {
			cell.append(raw.slice(at, found));
		}
		const mark = document.createElement("mark");
		mark.textContent = raw.slice(found, found + term.length);
		cell.append(mark);
		at = found + term.length;
	}
	if (at < raw.length) {
		cell.append(raw.slice(at));
	}
}

function applySearch(all) {
	const term = el("search").value;
	for (const line of el("lines").children) {
		if (all) {
			delete line.dataset.marked;
		}
		markLine(line, term);
	}
	countHits();
}

/* The list is read back off the page rather than remembered, because lines
   leave the top as new ones arrive: a remembered index would eventually point
   at a line that is no longer here. Which hit is current survives as a class
   on the element itself, which travels with it or leaves with it. */
function countHits() {
	hits = [...el("lines").querySelectorAll("mark")];
	hitAt = hits.findIndex((mark) => mark.classList.contains("current"));
	const term = el("search").value;
	el("hitButtons").hidden = !term;
	if (!term) {
		el("found").textContent = "";
		return;
	}
	if (hits.length === 0) {
		el("found").textContent = "none";
		return;
	}
	el("found").textContent = hitAt < 0
		? String(hits.length)
		: (hitAt + 1) + "/" + hits.length;
}

function jumpHit(by) {
	if (hits.length === 0) {
		return;
	}
	if (hitAt >= 0) {
		hits[hitAt].classList.remove("current");
	} else if (by < 0) {
		// Coming from nowhere backwards means the last one, not the one before
		// the first.
		hitAt = hits.length;
	}
	hitAt = (hitAt + by + hits.length) % hits.length;
	hits[hitAt].classList.add("current");
	/* Going to a hit is leaving the end of the stream, and the sentinel says so
	   on its own: the pane stops following until the reader comes back down. */
	hits[hitAt].scrollIntoView({ block: "center" });
	countHits();
}

el("search").oninput = () => applySearch(true);
el("search").onkeydown = (event) => {
	if (event.key === "Enter") {
		event.preventDefault();
		jumpHit(event.shiftKey ? -1 : 1);
	}
};
el("nextHit").onclick = () => jumpHit(1);
el("prevHit").onclick = () => jumpHit(-1);

/* Whether the reader is at the end of the stream: is the sentinel at the bottom
   of the list on screen?

   Asked of the layout directly rather than through an IntersectionObserver,
   which was the first shape of this and is wrong here for a reason worth
   keeping: an observer does not run while the page is not being rendered, and
   a panel whose window is minimised or behind another one is exactly that.
   Its callback would simply never fire, the last answer would stand for as
   long as the window stayed hidden, and nothing would say so. This is a
   question about geometry, the geometry is there to be read, and reading it
   costs nothing on a pane that updates once a second.

   The sentinel rather than scrollHeight arithmetic because that arithmetic
   rounds differently per browser and zoom, and is wrong by a pixel exactly
   when somebody is sitting at the bottom waiting to be followed. */
let movedLog = false;

function atTail() {
	const box = el("lineBox");
	const tail = el("tail");
	if (!box || !tail) {
		return true;
	}
	// One pixel of slack: the sentinel is one pixel tall, and a fractional
	// layout can put its top a hair past the edge while it is still the thing
	// the reader is looking at.
	return tail.getBoundingClientRect().top <= box.getBoundingClientRect().bottom + 1;
}

/* Dragging on a chart chooses the window every chart then follows. */

function armPlots() {
	for (const wrap of document.querySelectorAll(".plot")) {
		if (wrap.armed) {
			continue;
		}
		wrap.armed = true;
		arm(wrap);
	}
}

function arm(wrap) {
	let start = null;
	let band = null;

	wrap.onmousedown = (event) => {
		start = event.offsetX;
		band = document.createElement("div");
		band.className = "band";
		wrap.appendChild(band);
		event.preventDefault();
	};
	wrap.onmousemove = (event) => {
		if (band === null) {
			return;
		}
		band.style.left = `${Math.min(start, event.offsetX)}px`;
		band.style.width = `${Math.abs(event.offsetX - start)}px`;
	};
	wrap.onmouseleave = () => {
		if (band) {
			band.remove();
			band = null;
			start = null;
		}
	};
	wrap.onmouseup = (event) => {
		if (band === null) {
			return;
		}
		band.remove();
		band = null;
		const content = el("content");
		const from = Number(content.dataset.from || 0);
		const span = Number(content.dataset.to || 0) - from;
		const one = from + (Math.min(start, event.offsetX) / wrap.clientWidth) * span;
		const other = from + (Math.max(start, event.offsetX) / wrap.clientWidth) * span;
		start = null;
		// A click is not a range: it would freeze the charts on an instant.
		if (!span || other - one < 5000) {
			return;
		}
		act(`range/${Math.round(one)}/${Math.round(other)}`);
	};
	wrap.ondblclick = () => {
		act("range/live");
	};
}

/* The command dialog: the panel says what to run and never runs it. */

document.body.addEventListener("click", (event) => {
	const command = event.target.dataset ? event.target.dataset.command : "";
	if (!command) {
		return;
	}
	event.stopPropagation();
	showCommand(command);
});

function showCommand(command) {
	el("commandTitle").textContent = "The panel does not change anything";
	el("commandNote").textContent = "Run this where you keep your tree:";
	el("commandText").textContent = command;
	el("command").showModal();
}

// Called by the menu bar, which reaches the panel the same way a click does.
function about(text) {
	el("commandTitle").textContent = "hostctl";
	el("commandNote").textContent = "The panel is hostctl in another presentation.";
	el("commandText").textContent = text;
	el("command").showModal();
}

function pickWindow(seconds) {
	act(`window/${seconds}`);
}

function goLive() {
	act("range/live");
}

function goFleet() {
	act("select/fleet");
}

function focusSearch() {
	el("search").focus();
	el("search").select();
}

el("copy").onclick = async () => {
	try {
		await navigator.clipboard.writeText(el("commandText").textContent);
		el("copy").textContent = "Copied";
		setTimeout(() => { el("copy").textContent = "Copy"; }, 1200);
	} catch {
		// The text is selectable; saying nothing would be pretending it worked.
		el("copy").textContent = "Select it and copy";
	}
};
el("dismiss").onclick = () => el("command").close();

/* The two things that can be dragged. */

drag(el("grip"), "clientX", (start, delta) => {
	document.documentElement.style.setProperty("--sidebarWidth", `${clamp(start + delta, 170, 420)}px`);
});
drag(el("split"), "clientY", (start, delta) => {
	document.documentElement.style.setProperty("--logHeight", `${clamp(start - delta, 90, window.innerHeight - 260)}px`);
});

function drag(handle, axis, apply) {
	handle.onmousedown = (event) => {
		const origin = event[axis];
		const start = axis === "clientX" ? el("sidebar").clientWidth : el("console").clientHeight;
		const move = (moved) => apply(start, moved[axis] - origin);
		const stop = () => {
			document.removeEventListener("mousemove", move);
			document.removeEventListener("mouseup", stop);
			document.body.style.cursor = "";
		};
		document.addEventListener("mousemove", move);
		document.addEventListener("mouseup", stop);
		document.body.style.cursor = axis === "clientX" ? "col-resize" : "row-resize";
		event.preventDefault();
	};
}

function clamp(value, low, high) {
	return Math.max(low, Math.min(high, value));
}

/* Where the tree is depends on how much room there is, and the operator's
   choice is remembered per width: putting it away on a wide window says
   nothing about what a phone-sized one should do, and the other way round. */

const NARROW = window.matchMedia("(max-width: 620px)");
let awayWide = false;  // the operator hid it on a window with room for it
let awayNarrow = true; // narrow starts with the tree put away, as a phone does

function showNav() {
	const narrow = NARROW.matches;
	document.body.classList.toggle("narrow", narrow);
	document.body.classList.toggle("hideNav", narrow ? awayNarrow : awayWide);
}

function toggleNav() {
	if (NARROW.matches) {
		awayNarrow = !awayNarrow;
	} else {
		awayWide = !awayWide;
	}
	showNav();
}

NARROW.addEventListener("change", () => {
	if (NARROW.matches) {
		// Arriving at phone width, the tree gets out of the way on its own.
		awayNarrow = true;
	}
	showNav();
});
showNav();

// The first paint opens at the end of the stream, the way a terminal does.
settle(true);

// The menu bar reaches the panel the same way a click does.
function refreshNow() {
	act("refresh");
}
