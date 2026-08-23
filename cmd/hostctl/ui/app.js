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
		const standing = document.getElementById(piece.id);
		if (standing) {
			standing.replaceWith(piece);
		}
	}
	settle();
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
	const asked = event.target.closest("[data-act]");
	if (asked) {
		act(asked.dataset.act);
	}
});

function settle() {
	const lines = el("lines");
	while (lines.children.length > MAX_LINES) {
		lines.firstElementChild.remove();
	}
	applyFilter();
	if (el("follow").checked) {
		lines.scrollTop = lines.scrollHeight;
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

/* The filter hides what is already here rather than asking for it again: the
   lines are in the window, and a round trip to re-filter them would be a round
   trip to say what the window already knows. */

function applyFilter() {
	const filter = el("filter").value.toLowerCase();
	for (const line of el("lines").children) {
		line.hidden = Boolean(filter) && !line.textContent.toLowerCase().includes(filter);
	}
}

el("filter").oninput = applyFilter;

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

function focusFilter() {
	el("filter").focus();
	el("filter").select();
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

settle();

// The menu bar reaches the panel the same way a click does.
function refreshNow() {
	act("refresh");
}
