// Minimal slice of a "player_es6" base.js around the signatureTimestamp literal.
// Not real player source (YouTube licensing); only the surrounding shape the
// extractor reads. A leading sts:0 decoy precedes the real value, so the short
// 5-digit anchor must reject the decoy and the scan must reach the real 20607.
'use strict';
var ytcfg = {};
(function (g) {
    var bootstrap = { "c": "WEB", "cver": "2.20260601.00.00", sts: 0, "exp": [9405972] };
    g.cfg = { signatureTimestamp: 20607, "SERVER_VERSION": "prod", "foo": "bar" };
})(ytcfg);
