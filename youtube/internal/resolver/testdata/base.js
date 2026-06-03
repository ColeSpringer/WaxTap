// Authored, minimal stand-in for YouTube's base.js. This is NOT real YouTube
// code: it reproduces only the *shape* WaxTap's locators target so the cipher
// extraction can be tested offline. A real base.js is never committed (licensing).
//
// dcr is the signature transform: it splits its argument, applies a few helper
// operations, and re-joins. nfn is the n-parameter (throttling) transform.
// dcr("ABCDEFGH") === "GFEDH"; nfn("12345") === "54321".
var Xq = {
    rv: function(a) { a.reverse() },
    sp: function(a, b) { a.splice(0, b) },
    sw: function(a, b) { var c = a[0]; a[0] = a[b % a.length]; a[b % a.length] = c }
};
function dcr(a) {
    a = a.split("");
    Xq.sp(a, 2);
    Xq.sw(a, 5);
    Xq.rv(a);
    Xq.sp(a, 1);
    return a.join("")
}
var nfn = function(a) {
    var b = a.split("");
    b.reverse();
    return b.join("")
};
// Call sites, mimicking what the player runtime executes. The locators key off
// these, not off the definitions above, in the primary patterns.
function resolve(u) {
    var s = u.params.s;
    s && (s = dcr(decodeURIComponent(s)));
    u.set("sig", encodeURIComponent(s));
    var cd = u.params;
    (cd.get("n")) && (cd = nfn(cd.get("n")));
    return u
}
