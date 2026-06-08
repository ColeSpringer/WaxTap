// Synthetic stand-in for YouTube's base.js, exercising the whole-player solver
// (solver.go) without committing real player source. It is wrapped in the player
// IIFE; the descrambler is located by its direct alr/yes statement and driven
// through a URL-like object exactly as the real driver is.
//
// realDesc transforms by reversing, so the goldens are:
//   decodeN("12345")            === "54321"
//   decipherSignature("ABCDEFGH") === "HGFEDCBA"
//
// The decoys lock in discovery precision and consensus behavior:
//   throwDesc   - matched by the alr/yes fingerprint but throws when driven, so
//                 the consensus solver must skip it rather than fail.
//   nestedDecoy - its alr/yes call is nested in an if, not a direct body
//                 statement, so it must NOT be treated as a descrambler.
//   getNDecoy   - contains the literal get("n") the old locators keyed on, but
//                 no alr/yes, so it is not a descrambler either.
var SYNTH = {};
(function (g) {
    'use strict';
    var pconf = { signatureTimestamp: 19834 };
    var helper = { conf: function (a, b) { return a + b } };

    // A minimal URL-like object. The driver constructs it, sets "n", invokes the
    // lone method that is not constructor/set/get/clone, then reads "n"/"s" back.
    function YUrl(raw, key, val) {
        this.params = {};
        if (key !== undefined && val !== undefined) this.params[key] = val;
    }
    YUrl.prototype.set = function (k, v) { this.params[k] = v };
    YUrl.prototype.get = function (k) { return this.params[k] };
    YUrl.prototype.clone = function () { return this };
    YUrl.prototype.descramble = function () {
        if (this.params.n !== undefined) this.params.n = rev(String(this.params.n));
        if (this.params.s !== undefined) this.params.s = rev(String(this.params.s));
    };

    function rev(x) { return x.split("").reverse().join("") }

    // The real descrambler: a direct body statement is helper.conf("alr","yes").
    function realDesc(raw, key, val) {
        helper.conf("alr", "yes");
        return new YUrl(raw, key, val);
    }

    function throwDesc(raw, key, val) {
        helper.conf("alr", "yes");
        throw new Error("decoy descrambler");
    }

    function nestedDecoy(raw, key, val) {
        if (raw) { helper.conf("alr", "yes") }
        return new YUrl(raw, key, val);
    }

    function getNDecoy(u) {
        return u.get("n");
    }

    g.realDesc = realDesc;
    g.throwDesc = throwDesc;
    g.nestedDecoy = nestedDecoy;
    g.getNDecoy = getNDecoy;
    g.cfg = pconf;
})(SYNTH);
