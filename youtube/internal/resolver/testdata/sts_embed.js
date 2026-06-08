// Minimal slice of an embed-fallback base.js around the signatureTimestamp
// literal. Not real player source (YouTube licensing); only the surrounding
// shape the extractor reads. The real value comes first and a trailing sts:0
// decoy follows it, so the scan must return the real value and not be confused
// by a later decoy.
'use strict';
var ytcfg = {};
(function (g) {
    g.cfg = { "c": "WEB_EMBEDDED_PLAYER", signatureTimestamp: 20611, "embed": true };
    var teardown = { "reason": "none", sts: 0 };
})(ytcfg);
