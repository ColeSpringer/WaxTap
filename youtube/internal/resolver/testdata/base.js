// Minimal stand-in for YouTube's base.js. It contains only the shapes WaxTap's
// locators target so cipher extraction can be tested offline. The real player
// source is not committed for licensing reasons.
//
// dcr is the signature transform: it splits its argument, applies a few helper
// operations, and re-joins. nfn is the n-parameter (throttling) transform.
// dcr("ABCDEFGH") === "GFEDH"; nfn("12345") === "54321".
//
// Both transforms reference a top-level dependency (dcr the helper object Xq, nfn
// the lookup global nDigits), so cipher extraction must bundle the full
// dependency closure, not just the function body, to compile and run them.
//
// The player config also carries the signature timestamp sent to /player.
var cfg = { signatureTimestamp: 19834, foo: "bar" };
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
var nDigits = "0123456789";
var nfn = function(a) {
    var b = a.split("");
    for (var i = 0; i < b.length; i++) b[i] = nDigits.charAt(parseInt(b[i], 10));
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
