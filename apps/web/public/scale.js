// One pre-paint store for the user's UI scale. The scale is applied as the root
// font-size (index.css), never as CSS zoom, so layout, portals and pointer
// coordinates stay in one coordinate system.
;(function () {
  if (window.OpenGTMScale) return
  var key = "opengtm-ui-scale"
  var listeners = new Set()
  function normalize(value) {
    if (value === "85") return "90" // legacy "Compact"
    return value === "90" || value === "100" || value === "110" ? value : "auto"
  }
  function read() {
    try { return normalize(window.localStorage.getItem(key)) }
    catch { return "auto" }
  }
  var preference = read()
  var resolved = 100
  function update() {
    resolved = preference === "auto" ? 100 : Number(preference)
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
  window.addEventListener("storage", function (event) {
    if (event.key === key || event.key === null) {
      preference = read()
      update()
    }
  })
  update()
})()
