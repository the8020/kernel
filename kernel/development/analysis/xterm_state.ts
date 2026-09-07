// Phase 1 qualification prototype, deliberately outside deployed package code.
// The stock addon loses parser and other continuation state. This experiment
// captures data in the pinned xterm 5.5 engine without replaying terminal bytes.
// It is not yet a validated wire format or a production addon.
// deno-lint-ignore-file no-explicit-any

type Palette = { colors: number[]; defaults: number[] };
const headlessPalettes = new WeakMap<object, Palette>();

function rgba(r: number, g: number, b: number) { return ((r << 24) | (g << 16) | (b << 8) | 255) >>> 0; }

// Headless xterm publishes color requests but has no browser theme service to
// answer them. The terminal owner must supply that state and answer while detached.
export function installHeadlessColors(terminal: any) {
  const colors = ["2e3436", "cc0000", "4e9a06", "c4a000", "3465a4", "75507b", "06989a", "d3d7cf",
    "555753", "ef2929", "8ae234", "fce94f", "729fcf", "ad7fa8", "34e2e2", "eeeeec"].map((hex) =>
      ((parseInt(hex, 16) << 8) | 255) >>> 0);
  const levels = [0, 95, 135, 175, 215, 255];
  for (const r of levels) for (const g of levels) for (const b of levels) colors.push(rgba(r, g, b));
  for (let i = 0; i < 24; i++) colors.push(rgba(8 + i * 10, 8 + i * 10, 8 + i * 10));
  colors.push(rgba(255, 255, 255), rgba(0, 0, 0), rgba(255, 255, 255));
  const palette = { colors, defaults: [...colors] };
  headlessPalettes.set(terminal, palette);
  terminal._core._inputHandler.onColor((requests: any[]) => {
    for (const request of requests) {
      const index = request.index;
      if (request.type === 0) {
        const color = palette.colors[index]!;
        const channels = [color >>> 24, (color >>> 16) & 255, (color >>> 8) & 255];
        const identifier = index < 256 ? `4;${index}` : String(index - 256 + 10);
        terminal._core.coreService.triggerDataEvent(`\x1b]${identifier};rgb:${channels.map((x) => (x * 257).toString(16).padStart(4, "0")).join("/")}\x1b\\`);
      } else if (request.type === 1) {
        palette.colors[index] = rgba(request.color[0], request.color[1], request.color[2]);
      } else if (request.type === 2) {
        if (index === undefined) palette.colors.splice(0, 256, ...palette.defaults.slice(0, 256));
        else palette.colors[index] = palette.defaults[index]!;
      }
    }
  });
}

function paletteOf(terminal: any): Palette | undefined {
  const palette = headlessPalettes.get(terminal);
  if (palette) return { colors: [...palette.colors], defaults: [...palette.defaults] };
  const theme = terminal._core._themeService;
  if (!theme) return undefined;
  const values = (x: any) => [...x.ansi.map((color: any) => color.rgba), x.foreground.rgba, x.background.rgba, x.cursor.rgba];
  return { colors: values(theme.colors), defaults: values(theme._restoreColors) };
}

function restorePalette(terminal: any, saved: Palette | undefined) {
  if (!saved) return;
  const palette = headlessPalettes.get(terminal);
  if (palette) { palette.colors = [...saved.colors]; palette.defaults = [...saved.defaults]; return; }
  const theme = terminal._core._themeService;
  if (!theme) throw new Error("terminal palette owner is unavailable");
  const color = (rgba: number) => ({ rgba, css: `#${(rgba >>> 8).toString(16).padStart(6, "0")}` });
  const apply = (target: any, values: number[]) => {
    target.ansi = values.slice(0, 256).map(color);
    target.foreground = color(values[256]!);
    target.background = color(values[257]!);
    target.cursor = color(values[258]!);
  };
  apply(theme._restoreColors, saved.defaults);
  theme.modifyColors((target: any) => apply(target, saved.colors));
}

function attrs(value: any) {
  return { fg: value.fg, bg: value.bg, ext: value.extended._ext, url: value.extended._urlId };
}

function restoreAttrs(value: any, saved: any) {
  value.fg = saved.fg;
  value.bg = saved.bg;
  value.extended._ext = saved.ext;
  value.extended._urlId = saved.url;
}

function params(value: any) {
  return {
    params: [...value.params], sub: [...value._subParams], indices: [...value._subParamsIdx],
    length: value.length, subLength: value._subParamsLength,
    rejectDigits: value._rejectDigits, rejectSubDigits: value._rejectSubDigits, digitIsSub: value._digitIsSub,
  };
}

function restoreParams(value: any, saved: any) {
  value.params.set(saved.params);
  value._subParams.set(saved.sub);
  value._subParamsIdx.set(saved.indices);
  value.length = saved.length;
  value._subParamsLength = saved.subLength;
  value._rejectDigits = saved.rejectDigits;
  value._rejectSubDigits = saved.rejectSubDigits;
  value._digitIsSub = saved.digitIsSub;
}

const geometryKeys = ["x", "y", "ybase", "ydisp", "scrollTop", "scrollBottom", "savedX", "savedY"];

function buffer(value: any) {
  return {
    geometry: Object.fromEntries(geometryKeys.map((key) => [key, value[key]])),
    tabs: { ...value.tabs }, savedAttrs: attrs(value.savedCurAttrData), savedCharset: value.savedCharset,
    lines: Array.from({ length: value.lines.length }, (_, index) => {
      const line = value.lines.get(index);
      return {
        wrapped: line.isWrapped, length: line.length, data: [...line._data.subarray(0, line.length * 3)], combined: { ...line._combined },
        extended: Object.fromEntries(Object.entries(line._extendedAttrs).map(([key, a]: [string, any]) =>
          [key, { ext: a._ext, url: a._urlId }])),
      };
    }),
  };
}

function restoreBuffer(value: any, saved: any, attrTemplate: any) {
  value.clearAllMarkers();
  value.lines.length = 0;
  Object.assign(value, saved.geometry);
  value.tabs = { ...saved.tabs };
  value.savedCharset = saved.savedCharset;
  restoreAttrs(value.savedCurAttrData, saved.savedAttrs);
  for (const line of saved.lines) {
    const restored = value.getBlankLine(attrTemplate, line.wrapped);
    restored.length = line.length;
    restored._data = new Uint32Array(line.data);
    restored._combined = { ...line.combined };
    restored._extendedAttrs = {};
    for (const [key, data] of Object.entries(line.extended) as [string, any][]) {
      const extended = attrTemplate.extended.clone();
      extended._ext = data.ext;
      extended._urlId = data.url;
      restored._extendedAttrs[key] = extended;
    }
    value.lines.push(restored);
  }
}

function handlers(value: any[]) {
  return value.map((handler) => ({
    data: handler._data,
    hitLimit: handler._hitLimit,
    ...(handler._params ? { params: params(handler._params) } : {}),
  }));
}

function restoreHandlers(value: any[], saved: any[]) {
  if (value.length !== saved.length) throw new Error("xterm parser handlers differ");
  value.forEach((handler, index) => {
    handler._data = saved[index].data;
    handler._hitLimit = saved[index].hitLimit;
    if (saved[index].params) {
      handler._params = handler._params.clone();
      restoreParams(handler._params, saved[index].params);
    }
  });
}

export function captureTerminal(terminal: any) {
  const c = terminal._core;
  const b = c._bufferService.buffers;
  const h = c._inputHandler;
  const p = h._parser;
  if (p._oscParser._stack.paused || p._dcsParser._stack.paused || h._parseStack.paused) {
    throw new Error("flush xterm writes before capturing state");
  }
  const charset = c._charsetService;
  const snapshot = {
    version: "xterm-5.5.0-state-probe-1",
    cols: terminal.cols, rows: terminal.rows, scrollback: terminal.options.scrollback,
    normal: buffer(b.normal), alternate: buffer(b.alt), active: b.active === b.alt ? "alternate" : "normal",
    modes: c.coreService.modes, decModes: c.coreService.decPrivateModes,
    hidden: c.coreService.isCursorHidden, initialized: c.coreService.isCursorInitialized,
    cursorStyle: terminal.options.cursorStyle, cursorBlink: terminal.options.cursorBlink,
    palette: paletteOf(terminal),
    charset: { glevel: charset.glevel, charset: charset.charset, charsets: charset._charsets },
    mouse: { protocol: c.coreMouseService.activeProtocol, encoding: c.coreMouseService.activeEncoding },
    attr: attrs(h._curAttrData), erase: attrs(h._eraseAttrDataInternal),
    title: h._windowTitle, icon: h._iconName, titles: h._windowTitleStack, icons: h._iconNameStack,
    stringInterim: h._stringDecoder._interim, utf8Interim: [...h._utf8Decoder.interim],
    parser: {
      state: p.currentState, initial: p.initialState, collect: p._collect, preceding: p.precedingJoinState,
      params: params(p._params),
      osc: { id: p._oscParser._id, state: p._oscParser._state, active: handlers(p._oscParser._active) },
      dcs: { id: p._dcsParser._ident, active: handlers(p._dcsParser._active) },
    },
    links: [...c._oscLinkService._dataByLinkId.values()].map((entry: any) => ({
      id: entry.id, key: entry.key, data: entry.data,
      lines: entry.lines.map((marker: any) => ({
        buffer: b.normal.markers.includes(marker) ? "normal" : "alternate", line: marker.line,
      })),
    })),
    nextLinkId: c._oscLinkService._nextId,
  };
  // Use the actual JSON boundary rather than preserving hidden JS references.
  return JSON.parse(JSON.stringify(snapshot));
}

export function restoreTerminal(terminal: any, snapshot: any) {
  if (snapshot.version !== "xterm-5.5.0-state-probe-1") throw new Error("unknown state version");
  terminal.options.scrollback = snapshot.scrollback;
  terminal.resize(snapshot.cols, snapshot.rows);
  terminal.reset();
  const c = terminal._core;
  const b = c._bufferService.buffers;
  const h = c._inputHandler;
  restoreBuffer(b.normal, snapshot.normal, h._curAttrData);
  restoreBuffer(b.alt, snapshot.alternate, h._curAttrData);
  const before = b.active;
  b._activeBuffer = snapshot.active === "alternate" ? b.alt : b.normal;
  b._onBufferActivate.fire({ activeBuffer: b.active, inactiveBuffer: before });
  c.coreService.modes = { ...snapshot.modes };
  c.coreService.decPrivateModes = { ...snapshot.decModes };
  c.coreService.isCursorHidden = snapshot.hidden;
  c.coreService.isCursorInitialized = snapshot.initialized;
  terminal.options.cursorStyle = snapshot.cursorStyle;
  terminal.options.cursorBlink = snapshot.cursorBlink;
  restorePalette(terminal, snapshot.palette);
  c._charsetService.glevel = snapshot.charset.glevel;
  c._charsetService.charset = snapshot.charset.charset;
  c._charsetService._charsets = snapshot.charset.charsets;
  c.coreMouseService.activeProtocol = snapshot.mouse.protocol;
  c.coreMouseService.activeEncoding = snapshot.mouse.encoding;
  restoreAttrs(h._curAttrData, snapshot.attr);
  restoreAttrs(h._eraseAttrDataInternal, snapshot.erase);
  h._windowTitle = snapshot.title;
  h._iconName = snapshot.icon;
  h._windowTitleStack = [...snapshot.titles];
  h._iconNameStack = [...snapshot.icons];
  h._stringDecoder._interim = snapshot.stringInterim;
  h._utf8Decoder.interim.set(snapshot.utf8Interim);
  const p = h._parser;
  p.currentState = snapshot.parser.state;
  p.initialState = snapshot.parser.initial;
  p._collect = snapshot.parser.collect;
  p.precedingJoinState = snapshot.parser.preceding;
  restoreParams(p._params, snapshot.parser.params);
  const osc = snapshot.parser.osc;
  p._oscParser._id = osc.id;
  p._oscParser._state = osc.state;
  p._oscParser._active = osc.active.length ? p._oscParser._handlers[osc.id] ?? [] : [];
  restoreHandlers(p._oscParser._active, osc.active);
  const dcs = snapshot.parser.dcs;
  p._dcsParser._ident = dcs.id;
  p._dcsParser._active = dcs.active.length ? p._dcsParser._handlers[dcs.id] ?? [] : [];
  restoreHandlers(p._dcsParser._active, dcs.active);
  const links = c._oscLinkService;
  links._dataByLinkId.clear();
  links._entriesWithId.clear();
  links._nextId = snapshot.nextLinkId;
  for (const saved of snapshot.links) {
    const entry = { id: saved.id, key: saved.key, data: saved.data, lines: [] as any[] };
    links._dataByLinkId.set(entry.id, entry);
    if (entry.key !== undefined) links._entriesWithId.set(entry.key, entry);
    for (const line of saved.lines) {
      const owner = line.buffer === "normal" ? b.normal : b.alt;
      const marker = owner.addMarker(line.line);
      entry.lines.push(marker);
      marker.onDispose(() => links._removeMarkerFromLink(entry, marker));
    }
  }
  terminal.refresh?.(0, terminal.rows - 1);
}
