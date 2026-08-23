// The panel reads; it never changes anything. An action is shown as the hostctl
// command that would do it, because a dashboard that acts is a dashboard that
// acts by accident.

const MAX_LINES = 4000; // a window that grows without bound is a window that dies

const state = {
	since: {},                        // last log sequence seen, per machine
	lines: [],                        // merged, oldest first
	fleet: [],
	window: 3600,
	from: 0,                          // a range dragged on a chart; 0 is live
	to: 0,
	range: { from: 0, to: 0 },        // what the charts are drawn against
	filter: "",
	follow: true,
	selection: { kind: "fleet" },
	open: {},
	refreshing: false,
};

const el = (id) => document.getElementById(id);

/* What the answer means. */

function machine(name) {
	return state.fleet.find((host) => host.host === name);
}

function serviceOf(hostName, name) {
	const host = machine(hostName);
	return (host ? host.services || [] : []).find((service) => service.name === name);
}

function seriesOf(host, scope, name, metric) {
	for (const series of (host || {}).metrics || []) {
		if (series.scope === scope && series.name === name && series.metric === metric) {
			return series.points || [];
		}
	}
	return [];
}

function servicesIn(host, metric) {
	const names = new Set();
	for (const series of (host || {}).metrics || []) {
		if (series.scope === "service" && series.metric === metric && (series.points || []).length) {
			names.add(series.name);
		}
	}
	return [...names].sort();
}

function last(points) {
	return points.length ? points[points.length - 1].value : null;
}

function ratio(used, total) {
	const whole = last(total);
	if (!used.length || !whole) {
		return [];
	}
	return used.map((point) => ({ "time-ms": point["time-ms"], value: (point.value / whole) * 100 }));
}

/* Talking to the daemon. */

async function refresh() {
	if (state.refreshing) {
		return; // a slow fleet must not queue refreshes behind itself
	}
	state.refreshing = true;
	try {
		const fleet = await window.hostd.fleet(state.window, state.from, state.to, state.since);
		state.fleet = fleet;
		state.range = state.from ? { from: state.from, to: state.to } :
			{ from: Date.now() - state.window * 1000, to: Date.now() };
		absorb(fleet);
		const missing = fleet.filter((host) => host.error);
		say(missing.length ? `${missing.length} machine(s) not answering` : `updated ${clock(new Date())}`, missing.length);
		renderAll();
	} catch (err) {
		say(String(err), true);
	} finally {
		state.refreshing = false;
	}
}

function absorb(fleet) {
	for (const host of fleet) {
		if (host.since > (state.since[host.host] || 0)) {
			state.since[host.host] = host.since;
		}
		for (const line of host.lines || []) {
			state.lines.push({ ...line, host: host.host });
		}
	}
	state.lines.sort((a, b) => a["time-ms"] - b["time-ms"]);
	if (state.lines.length > MAX_LINES) {
		state.lines = state.lines.slice(-MAX_LINES);
	}
}

function say(text, bad) {
	const node = el("state");
	node.textContent = text;
	node.classList.toggle("bad", Boolean(bad));
}

/* The source list. */

function renderAll() {
	renderTree();
	renderContent();
	renderLines();
	renderRange();
}

function renderTree() {
	const tree = el("tree");
	tree.innerHTML = "";
	tree.appendChild(leaf({
		label: "Fleet", className: "row machine", selected: state.selection.kind === "fleet",
		meta: `${state.fleet.length}`,
		onPick: () => select({ kind: "fleet" }),
	}));

	const header = document.createElement("li");
	header.className = "section";
	header.textContent = "Machines";
	tree.appendChild(header);

	for (const host of state.fleet) {
		const services = host.services || [];
		const open = state.open[host.host] !== false;
		const item = document.createElement("li");
		item.className = "group";
		item.appendChild(leaf({
			label: host.host,
			className: "row machine",
			selected: state.selection.kind === "host" && state.selection.host === host.host,
			dot: host.error ? "unreachable" : worst(services),
			meta: host.error ? "!" : `${services.filter((s) => s.state === "running").length}/${services.length}`,
			twist: services.length ? (open ? "open" : "") : null,
			onTwist: () => {
				state.open[host.host] = !open;
				renderTree();
			},
			onPick: () => select({ kind: "host", host: host.host }),
		}));
		if (open) {
			for (const service of services) {
				item.appendChild(leaf({
					label: service.name,
					className: "row child",
					selected: state.selection.kind === "service" &&
						state.selection.host === host.host && state.selection.name === service.name,
					dot: service.state,
					meta: service.every || "",
					onPick: () => select({ kind: "service", host: host.host, name: service.name }),
				}));
			}
		}
		tree.appendChild(item);
	}
}

function leaf(spec) {
	const li = document.createElement("li");
	const row = document.createElement("div");
	row.className = spec.className + (spec.selected ? " selected" : "");
	row.setAttribute("role", "treeitem");

	if (spec.twist !== null && spec.twist !== undefined) {
		const twist = document.createElement("span");
		twist.className = `twist ${spec.twist}`;
		twist.textContent = "▶";
		twist.onclick = (event) => {
			event.stopPropagation();
			spec.onTwist();
		};
		row.appendChild(twist);
	} else if (spec.className.includes("child")) {
		const pad = document.createElement("span");
		pad.className = "twist";
		row.appendChild(pad);
	}

	if (spec.dot) {
		const dot = document.createElement("span");
		dot.className = `dot ${spec.dot}`;
		row.appendChild(dot);
	}

	const name = document.createElement("span");
	name.className = "name";
	name.textContent = spec.label;
	row.appendChild(name);

	if (spec.meta) {
		const meta = document.createElement("span");
		meta.className = "meta";
		meta.textContent = spec.meta;
		row.appendChild(meta);
	}

	row.onclick = spec.onPick;
	li.appendChild(row);
	return li;
}

// The worst thing happening on a machine is what its one dot has to say.
function worst(services) {
	const order = ["failed", "starting", "stopped", "scheduled", "running"];
	let found = "running";
	for (const service of services) {
		if (order.indexOf(service.state) < order.indexOf(found)) {
			found = service.state;
		}
	}
	return services.length ? found : "stopped";
}

function select(selection) {
	state.selection = selection;
	renderAll();
}

/* The detail. */

function renderContent() {
	const content = el("content");
	content.innerHTML = "";
	if (state.selection.kind === "host") {
		return hostView(content, machine(state.selection.host));
	}
	if (state.selection.kind === "service") {
		return serviceView(content, machine(state.selection.host), serviceOf(state.selection.host, state.selection.name));
	}
	return fleetView(content);
}

function fleetView(content) {
	title("Fleet", `${state.fleet.length} machine(s)`);
	const grid = document.createElement("div");
	grid.className = "cards";
	for (const host of state.fleet) {
		const card = document.createElement("section");
		card.className = "card machineCard";
		card.onclick = () => select({ kind: "host", host: host.host });

		const heading = document.createElement("h2");
		heading.append(host.host);
		const aside = document.createElement("span");
		aside.className = "aside";
		const services = host.services || [];
		aside.textContent = host.error ? "not answering" :
			`${services.filter((s) => s.state === "running").length} of ${services.length} running`;
		heading.appendChild(aside);
		card.appendChild(heading);

		if (host.error) {
			const why = document.createElement("p");
			why.className = "problem";
			why.textContent = host.error;
			card.appendChild(why);
			grid.appendChild(card);
			continue;
		}

		card.appendChild(numbers(host));
		card.appendChild(plot(hostLayers(host), "short"));
		grid.appendChild(card);
	}
	if (!state.fleet.length) {
		grid.appendChild(nothing("no machine is listed in your inventory"));
	}
	content.appendChild(grid);
}

function numbers(host) {
	const cpu = last(seriesOf(host, "host", "", "cpu-percent"));
	const memory = ratio(seriesOf(host, "host", "", "memory-bytes"), seriesOf(host, "host", "", "memory-total-bytes"));
	const disk = ratio(seriesOf(host, "host", "", "disk-bytes"), seriesOf(host, "host", "", "disk-total-bytes"));
	const load = last(seriesOf(host, "host", "", "load-1"));

	const wrap = document.createElement("div");
	wrap.className = "stat";
	for (const [label, value] of [
		["cpu", percent(cpu)], ["memory", percent(last(memory))],
		["disk", percent(last(disk))], ["load", load === null ? "-" : load.toFixed(2)],
	]) {
		const box = document.createElement("div");
		const number = document.createElement("span");
		number.className = "value";
		number.textContent = value;
		const of = document.createElement("span");
		of.className = "of";
		of.textContent = label;
		box.append(number, of);
		wrap.appendChild(box);
	}
	return wrap;
}

function hostView(content, host) {
	if (!host) {
		return;
	}
	title(host.host, host.error ? host.error : `${(host.services || []).length} service(s) declared`);
	if (host.error) {
		content.appendChild(nothing(host.error));
		return;
	}

	const table = card("Services");
	table.appendChild(serviceTable(host));
	content.appendChild(table);

	const usage = card("Load");
	usage.appendChild(numbers(host));
	usage.appendChild(plot(hostLayers(host)));
	content.appendChild(usage);

	const names = servicesIn(host, "memory-bytes");
	if (!names.length) {
		return;
	}
	const stacked = card("Memory by service");
	stacked.appendChild(plot(names.map((name, index) => ({
		label: name,
		points: seriesOf(host, "service", name, "memory-bytes"),
		colour: hue(index),
		stack: true,
	})), "", bytes));
	content.appendChild(stacked);
}

function hostLayers(host) {
	const memory = ratio(seriesOf(host, "host", "", "memory-bytes"), seriesOf(host, "host", "", "memory-total-bytes"));
	return [
		{ label: "cpu", points: seriesOf(host, "host", "", "cpu-percent"), colour: "#007aff", area: true, top: 100 },
		{ label: "memory", points: memory, colour: "#ff9f0a", area: true, top: 100 },
	];
}

function serviceTable(host) {
	const table = document.createElement("table");
	const head = document.createElement("tr");
	for (const name of ["service", "state", "image", "uptime", "restarts", ""]) {
		const th = document.createElement("th");
		th.textContent = name;
		head.appendChild(th);
	}
	table.appendChild(head);

	for (const service of host.services || []) {
		const row = document.createElement("tr");
		const first = cell("");
		const link = document.createElement("span");
		link.textContent = service.name;
		first.appendChild(link);
		first.onclick = () => select({ kind: "service", host: host.host, name: service.name });
		row.appendChild(first);
		row.appendChild(stateCell(service));
		row.appendChild(cell(short(service.image), "dim"));
		row.appendChild(cell(service.every ? "-" : uptime(service["since-ms"])));
		row.appendChild(cell(String(service.restarts || 0)));
		row.appendChild(commandCell(host.host, service));
		table.appendChild(row);
		if (service["last-error"]) {
			// Attached to the service rather than kept a click away: a machine
			// worth opening is usually a machine with something wrong on it.
			const why = document.createElement("tr");
			const cause = cell(service["last-error"], "problem");
			cause.colSpan = 6;
			cause.className = "wide problem";
			why.appendChild(cause);
			table.appendChild(why);
		}
	}
	if (!(host.services || []).length) {
		const row = document.createElement("tr");
		row.appendChild(cell("no service is declared here", "dim"));
		table.appendChild(row);
	}
	return table;
}

function stateCell(service) {
	const td = document.createElement("td");
	const pill = document.createElement("span");
	pill.className = "pill";
	const dot = document.createElement("span");
	dot.className = `dot ${service.state}`;
	pill.appendChild(dot);
	let text = service.state;
	if (service.every && service.runs) {
		text += ` (${service.runs})`;
	}
	if (service.orphan) {
		text += " (orphan)";
	}
	pill.append(text);
	td.appendChild(pill);
	return td;
}

function commandCell(host, service) {
	const td = document.createElement("td");
	td.className = "actions";
	for (const verb of ["restart", "stop"]) {
		const button = document.createElement("button");
		button.className = "push";
		button.textContent = verb;
		button.onclick = (event) => {
			event.stopPropagation();
			showCommand(`hostctl -host ${host} service ${verb} ${service.name}`);
		};
		td.appendChild(button);
	}
	return td;
}

function serviceView(content, host, service) {
	if (!host || !service) {
		return;
	}
	title(service.name, `${host.host} · ${service.every ? `every ${service.every}` : service.state}`);

	const facts = card("Declaration");
	const table = document.createElement("table");
	table.className = "facts";
	const rows = [
		["state", service.state + (service.orphan ? " (running here, declared nowhere)" : "")],
		["image", service.image || "-"],
		["desired", service.desired || "-"],
	];
	if (service.every) {
		rows.push(["every", service.every], ["runs going", String(service.runs || 0)]);
	} else {
		rows.push(["uptime", uptime(service["since-ms"])], ["pid", service.pid ? String(service.pid) : "-"]);
	}
	rows.push(["restarts", String(service.restarts || 0)], ["last exit", String(service["last-exit"] || 0)]);
	if (service["last-error"]) {
		rows.push(["problem", service["last-error"]]);
	}
	for (const [label, value] of rows) {
		const row = document.createElement("tr");
		row.appendChild(cell(label, "dim"));
		const td = cell(value);
		td.className = "wide";
		row.appendChild(td);
		table.appendChild(row);
	}
	facts.appendChild(table);
	content.appendChild(facts);

	const cpu = seriesOf(host, "service", service.name, "cpu-percent");
	const memory = seriesOf(host, "service", service.name, "memory-bytes");
	if (cpu.length || memory.length) {
		const usage = card("Usage");
		usage.appendChild(plot([{ label: "cpu", points: cpu, colour: "#007aff", area: true }]));
		usage.appendChild(plot([{ label: "memory", points: memory, colour: "#ff9f0a", area: true }], "", bytes));
		content.appendChild(usage);
	}

	const actions = card("What the panel will not do for you");
	const line = document.createElement("div");
	line.className = "stat";
	for (const verb of ["restart", "stop", "start"]) {
		const button = document.createElement("button");
		button.className = "push";
		button.textContent = verb;
		button.onclick = () => showCommand(`hostctl -host ${host.host} service ${verb} ${service.name}`);
		line.appendChild(button);
	}
	actions.appendChild(line);
	content.appendChild(actions);
}

function title(text, subtitle) {
	el("title").textContent = text;
	el("subtitle").textContent = subtitle || "";
}

function card(heading) {
	const section = document.createElement("section");
	section.className = "card";
	const h = document.createElement("h2");
	h.textContent = heading;
	section.appendChild(h);
	return section;
}

function cell(text, className) {
	const td = document.createElement("td");
	td.textContent = text;
	if (className) {
		td.className = className;
	}
	return td;
}

function nothing(text) {
	const p = document.createElement("p");
	p.className = "empty";
	p.textContent = text;
	return p;
}

/* Charts. Time runs left to right with now on the right, which is the way
   somebody reads a graph they are watching. Dragging across one chooses a
   window, and every chart in the panel follows it. */

function plot(layers, className, format) {
	const wrap = document.createElement("div");
	wrap.className = `plot ${className || ""}`;

	const legend = document.createElement("div");
	legend.className = "legend";
	for (const layer of layers) {
		const key = document.createElement("span");
		key.className = "key";
		const swatch = document.createElement("span");
		swatch.className = "swatch";
		swatch.style.background = layer.colour;
		key.appendChild(swatch);
		const value = last(layer.points);
		key.append(`${layer.label} ${value === null ? "-" : (format || percent)(value)}`);
		legend.appendChild(key);
	}

	const canvas = document.createElement("canvas");
	wrap.appendChild(canvas);
	armDrag(wrap);

	const holder = document.createElement("div");
	holder.appendChild(legend);
	holder.appendChild(wrap);
	requestAnimationFrame(() => draw(canvas, layers, format || percent));
	return holder;
}

function draw(canvas, layers, format) {
	const scale = window.devicePixelRatio || 1;
	const width = canvas.clientWidth;
	const height = canvas.clientHeight;
	if (!width || !height) {
		return;
	}
	canvas.width = width * scale;
	canvas.height = height * scale;
	const context = canvas.getContext("2d");
	context.scale(scale, scale);
	context.clearRect(0, 0, width, height);

	const from = state.range.from;
	const to = state.range.to;
	const at = (ms) => ((ms - from) / (to - from || 1)) * width;

	const stacked = layers.filter((layer) => layer.stack);
	let top = 0;
	for (const layer of layers) {
		if (layer.top) {
			top = Math.max(top, layer.top);
			continue;
		}
		for (const point of layer.points) {
			top = Math.max(top, point.value);
		}
	}
	if (stacked.length) {
		top = Math.max(top, stackedTop(stacked));
	}
	top = top || 1;

	context.strokeStyle = getComputedStyle(document.body).getPropertyValue("--grid").trim();
	context.lineWidth = 1;
	for (const fraction of [0.25, 0.5, 0.75, 1]) {
		const y = height - height * fraction + 0.5;
		context.beginPath();
		context.moveTo(0, y);
		context.lineTo(width, y);
		context.stroke();
	}

	if (stacked.length) {
		drawStack(context, stacked, width, height, at, top);
	}
	for (const layer of layers) {
		if (layer.stack || layer.points.length < 2) {
			continue;
		}
		const line = new Path2D();
		layer.points.forEach((point, index) => {
			const x = at(point["time-ms"]);
			const y = height - (Math.min(point.value, top) / top) * height;
			if (index === 0) {
				line.moveTo(x, y);
				return;
			}
			line.lineTo(x, y);
		});
		if (layer.area) {
			const area = new Path2D(line);
			area.lineTo(at(layer.points[layer.points.length - 1]["time-ms"]), height);
			area.lineTo(at(layer.points[0]["time-ms"]), height);
			area.closePath();
			const wash = context.createLinearGradient(0, 0, 0, height);
			wash.addColorStop(0, layer.colour + "44");
			wash.addColorStop(1, layer.colour + "00");
			context.fillStyle = wash;
			context.fill(area);
		}
		context.strokeStyle = layer.colour;
		context.lineWidth = 1.5;
		context.stroke(line);
	}

	context.fillStyle = getComputedStyle(document.body).getPropertyValue("--tertiary").trim();
	context.font = "10px -apple-system, sans-serif";
	context.fillText(format(top), 4, 11);
}

// One time axis for every series: a stack drawn on each series' own instants
// would put the layers on different x positions and add up to nothing.
function instantsOf(layers) {
	const all = new Set();
	for (const layer of layers) {
		for (const point of layer.points) {
			all.add(point["time-ms"]);
		}
	}
	return [...all].sort((a, b) => a - b);
}

function valueAt(points, when) {
	let value = 0;
	for (const point of points) {
		if (point["time-ms"] > when) {
			break;
		}
		value = point.value;
	}
	return value;
}

function stackedTop(layers) {
	let top = 0;
	for (const when of instantsOf(layers)) {
		let sum = 0;
		for (const layer of layers) {
			sum += valueAt(layer.points, when);
		}
		top = Math.max(top, sum);
	}
	return top;
}

function drawStack(context, layers, width, height, at, top) {
	const instants = instantsOf(layers);
	if (instants.length < 2) {
		return;
	}
	const floor = instants.map(() => 0);
	for (const layer of layers) {
		const region = new Path2D();
		instants.forEach((when, index) => {
			const y = height - (Math.min(floor[index] + valueAt(layer.points, when), top) / top) * height;
			const x = at(when);
			if (index === 0) {
				region.moveTo(x, y);
				return;
			}
			region.lineTo(x, y);
		});
		for (let index = instants.length - 1; index >= 0; index--) {
			region.lineTo(at(instants[index]), height - (Math.min(floor[index], top) / top) * height);
		}
		region.closePath();
		context.fillStyle = layer.colour + "cc";
		context.fill(region);
		context.strokeStyle = layer.colour;
		context.lineWidth = 0.75;
		context.stroke(region);
		instants.forEach((when, index) => {
			floor[index] += valueAt(layer.points, when);
		});
	}
}

const PALETTE = ["#007aff", "#ff9f0a", "#34c759", "#af52de", "#ff375f", "#5ac8fa", "#ffd60a", "#64d2ff"];

function hue(index) {
	return PALETTE[index % PALETTE.length];
}

function armDrag(wrap) {
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
		const left = Math.min(start, event.offsetX);
		band.style.left = `${left}px`;
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
		const width = wrap.clientWidth;
		const span = state.range.to - state.range.from;
		const one = state.range.from + (Math.min(start, event.offsetX) / width) * span;
		const other = state.range.from + (Math.max(start, event.offsetX) / width) * span;
		start = null;
		// A click is not a range: it would freeze the charts on an instant.
		if (other - one < 5000) {
			return;
		}
		state.from = Math.round(one);
		state.to = Math.round(other);
		refresh();
	};
	wrap.ondblclick = live;
}

function live() {
	state.from = 0;
	state.to = 0;
	refresh();
}

function renderRange() {
	const chosen = Boolean(state.from);
	el("range").hidden = !chosen;
	for (const button of el("windows").children) {
		button.classList.toggle("on", !chosen && Number(button.dataset.seconds) === state.window);
	}
	if (chosen) {
		el("rangeText").textContent = `${clock(new Date(state.from))} – ${clock(new Date(state.to))}`;
	}
}

/* The log, filtered by whatever is selected in the source list. */

function renderLines() {
	const list = el("lines");
	const filter = state.filter.toLowerCase();
	list.innerHTML = "";
	for (const line of state.lines) {
		if (!inSelection(line)) {
			continue;
		}
		const text = `${line.host} ${line.service} ${line.run} ${line.text}`;
		if (filter && !text.toLowerCase().includes(filter)) {
			continue;
		}
		list.appendChild(lineOf(line));
	}
	if (!list.children.length) {
		list.appendChild(nothing(state.filter ? "nothing matches the filter" : "nothing has been written yet"));
	}
	if (state.follow) {
		list.scrollTop = list.scrollHeight;
	}
}

function inSelection(line) {
	if (state.selection.kind === "host") {
		return line.host === state.selection.host;
	}
	if (state.selection.kind === "service") {
		return line.host === state.selection.host && line.service === state.selection.name;
	}
	return true;
}

function lineOf(line) {
	const item = document.createElement("li");
	if (line.stream === "err") {
		item.className = "err";
	}
	const when = document.createElement("span");
	when.className = "when";
	when.textContent = clock(new Date(line["time-ms"])) + " ";
	item.appendChild(when);

	const where = document.createElement("span");
	where.className = "where";
	let place = state.selection.kind === "fleet" ? `${line.host}/${line.service}` : line.service;
	if (line.run) {
		place += `/${line.run}`;
	}
	where.textContent = place + " ";
	item.appendChild(where);

	const text = document.createElement("span");
	text.className = line.stream === "event" ? "text event" : "text";
	text.textContent = line.text;
	item.appendChild(text);
	return item;
}

/* Formatting. */

function clock(date) {
	return date.toTimeString().slice(0, 8);
}

function percent(value) {
	return value === null ? "-" : `${value.toFixed(1)}%`;
}

function bytes(value) {
	if (value === null) {
		return "-";
	}
	const units = ["B", "K", "M", "G", "T"];
	let index = 0;
	while (value >= 1024 && index < units.length - 1) {
		value /= 1024;
		index++;
	}
	return `${value.toFixed(index ? 1 : 0)}${units[index]}`;
}

function short(image) {
	return (image || "").split("@")[0];
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

/* Chrome. */

function showCommand(command) {
	el("commandText").textContent = command;
	el("command").showModal();
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
el("rangeReset").onclick = live;

el("windows").onclick = (event) => {
	const seconds = Number(event.target.dataset.seconds || 0);
	if (!seconds) {
		return;
	}
	state.window = seconds;
	state.from = 0;
	state.to = 0;
	refresh();
};

el("filter").oninput = (event) => {
	state.filter = event.target.value;
	renderLines();
};
el("follow").onchange = (event) => {
	state.follow = event.target.checked;
};

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
			redrawCharts();
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

// Canvas keeps the pixels it was given, so a resized window redraws or shows a
// stretched picture of the past.
function redrawCharts() {
	renderContent();
}

window.addEventListener("resize", redrawCharts);

refresh();
setInterval(refresh, 2000);
