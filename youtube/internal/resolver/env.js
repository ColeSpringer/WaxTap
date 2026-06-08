// Browser-global stub for running YouTube's full base.js in goja.
//
// The whole player is executed (not carved up), so its load-time top-level code
// (feature detection, telemetry setup, timers, observers) runs and references
// browser globals goja does not provide. Each global below exists only to let
// that code run far enough to define the descrambler; none of it is the cipher.
//
// MAINTENANCE: this is an explicit, fail-loud allow-list, not a catch-all Proxy.
// When a rotated player throws `X is not defined`, the name surfaces in the
// ErrCipherSolve wrap and the resolver's warn logs; add one entry here and move
// on. Keep the network stubs (XMLHttpRequest, fetch, sendBeacon) inert: they
// must never invoke a callback or perform I/O, so the player's telemetry cannot
// spin or reach out from inside the sandbox.
var G = globalThis;

function noop() { return function () {}; }

G.location = {
    hash: "", host: "www.youtube.com", hostname: "www.youtube.com",
    href: "https://www.youtube.com/watch?v=waxtap",
    origin: "https://www.youtube.com", password: "", pathname: "/watch",
    port: "", protocol: "https:", search: "?v=waxtap", username: "",
    reload: noop(), replace: noop(), assign: noop()
};

// Inert network surface: open/send/setRequestHeader do nothing and no readystate
// or load callback ever fires.
if (typeof G.XMLHttpRequest === "undefined") {
    G.XMLHttpRequest = function () {};
    G.XMLHttpRequest.prototype = { open: noop(), send: noop(), setRequestHeader: noop() };
}
// A real but never-resolving Promise: it carries then/catch/finally (so a
// load-time fetch(...).finally(...) does not throw) yet no callback ever fires.
if (typeof G.fetch === "undefined") {
    G.fetch = function () { return new Promise(function () {}); };
}

if (typeof G.document === "undefined") {
    G.document = {
        createElement: function () {
            return { style: {}, setAttribute: noop(), appendChild: noop(), getContext: function () { return null; } };
        },
        getElementsByTagName: function () { return []; },
        getElementById: function () { return null; },
        querySelector: function () { return null; },
        querySelectorAll: function () { return []; },
        cookie: "", addEventListener: noop(),
        createEvent: function () { return { initEvent: noop() }; },
        documentElement: { style: {} }, head: { appendChild: noop() }, body: {}
    };
}

if (typeof G.navigator === "undefined") {
    // sendBeacon reports success without sending anything (inert).
    G.navigator = {
        userAgent: "Mozilla/5.0", appVersion: "5.0", platform: "Win32",
        language: "en-US", languages: ["en-US"], sendBeacon: function () { return true; },
        vendor: "", product: "Gecko", cookieEnabled: true
    };
}

if (typeof G.self === "undefined") { G.self = G; }
if (typeof G.window === "undefined") { G.window = G; }

// goja provides no console; base.js touches it during load/feature detection.
if (typeof G.console === "undefined") {
    G.console = {
        log: noop(), warn: noop(), error: noop(), info: noop(), debug: noop(),
        trace: noop(), assert: noop(), dir: noop(), group: noop(), groupEnd: noop(),
        groupCollapsed: noop(), table: noop(), time: noop(), timeEnd: noop(), count: noop()
    };
}

if (typeof G.Intl === "undefined") {
    G.Intl = {
        DateTimeFormat: function () {
            return { resolvedOptions: function () { return { timeZone: "UTC", locale: "en-US" }; }, format: function () { return ""; }, formatToParts: function () { return []; } };
        },
        NumberFormat: function () {
            return { resolvedOptions: function () { return {}; }, format: function (x) { return String(x); }, formatToParts: function () { return []; } };
        },
        Collator: function () { return { compare: function () { return 0; } }; },
        ListFormat: function () { return { format: function () { return ""; } }; },
        PluralRules: function () { return { select: function () { return "other"; } }; },
        getCanonicalLocales: function (x) { return [].concat(x || []); }
    };
    ["DateTimeFormat", "NumberFormat", "Collator", "ListFormat", "PluralRules", "RelativeTimeFormat", "Segmenter"].forEach(function (k) {
        if (G.Intl[k]) { G.Intl[k].supportedLocalesOf = function () { return []; }; }
    });
}

if (typeof G.setTimeout === "undefined") {
    G.setTimeout = function () { return 0; };
    G.clearTimeout = noop();
    G.setInterval = function () { return 0; };
    G.clearInterval = noop();
    G.requestAnimationFrame = function () { return 0; };
    G.cancelAnimationFrame = noop();
}
if (typeof G.queueMicrotask === "undefined") { G.queueMicrotask = function (f) { try { f(); } catch (e) {} }; }
if (typeof G.btoa === "undefined") { G.btoa = function (s) { return s; }; }
if (typeof G.atob === "undefined") { G.atob = function (s) { return s; }; }
if (typeof G.performance === "undefined") {
    G.performance = { now: function () { return 0; }, timing: {}, mark: noop(), measure: noop(), getEntriesByType: function () { return []; }, getEntriesByName: function () { return []; } };
}
if (typeof G.crypto === "undefined") { G.crypto = { getRandomValues: function (a) { return a; }, subtle: {} }; }
if (typeof G.TextEncoder === "undefined") {
    G.TextEncoder = function () { this.encode = function (s) { var a = []; for (var i = 0; i < (s || "").length; i++) a.push(s.charCodeAt(i) & 255); return a; }; };
}
if (typeof G.TextDecoder === "undefined") {
    G.TextDecoder = function () { this.decode = function (a) { var s = ""; a = a || []; for (var i = 0; i < a.length; i++) s += String.fromCharCode(a[i]); return s; }; };
}
if (typeof G.matchMedia === "undefined") {
    G.matchMedia = function () { return { matches: false, addListener: noop(), removeListener: noop(), addEventListener: noop(), removeEventListener: noop() }; };
}
if (typeof G.addEventListener === "undefined") { G.addEventListener = noop(); G.removeEventListener = noop(); }
if (typeof G.screen === "undefined") { G.screen = { width: 1920, height: 1080, availWidth: 1920, availHeight: 1040 }; }
if (typeof G.history === "undefined") { G.history = { pushState: noop(), replaceState: noop() }; }
if (typeof G.MutationObserver === "undefined") { G.MutationObserver = function () { return { observe: noop(), disconnect: noop() }; }; }
