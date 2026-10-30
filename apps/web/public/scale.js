// One pre-paint store for the dashboard and its portals. Use logical window
// size: Retina pixels and browser page zoom are not extra workspace area.
;(function () {
  if (window.OpenGTMScale) return
  var key = "opengtm-ui-scale"
  var coarse = window.matchMedia("(pointer: coarse)")
  var listeners = new Set()
  function normalize(value) {
    return value === "85" || value === "100" ? value : "auto"
  }
  function read() {
    try { return normalize(window.localStorage.getItem(key)) }
    catch { return "auto" }
  }
  var preference = read()
  var resolved = 100
  function update() {
    var width = window.outerWidth || window.innerWidth
    var height = window.outerHeight || window.innerHeight
    // Reflow and touch accessibility take precedence over compact density.
    var desktop = !coarse.matches && window.innerWidth >= 1024
    resolved = desktop && (preference === "85" ||
      (preference === "auto" && (width <= 1600 || height <= 900))) ? 85 : 100
    document.documentElement.style.setProperty("--gtm-ui-scale", String(resolved / 100))
    document.documentElement.dataset.uiScale = String(resolved)
    listeners.forEach(function (listener) { listener() })
  }
  window.OpenGTMScale = {
    getSnapshot: function () { return preference + ":" + resolved },
    setPreference: function (value) {
      preference = normalize(value)
      try { window.localStorage.setItem(key, preference) } catch { /* Storage can be unavailable. */ }
      update()
    },
    subscribe: function (listener) {
      listeners.add(listener)
      return function () { listeners.delete(listener) }
    },
  }
  window.addEventListener("resize", update)
  coarse.addEventListener("change", update)
  window.addEventListener("storage", function (event) {
    if (event.key === key || event.key === null) {
      preference = read()
      update()
    }
  })
  update()
})()
