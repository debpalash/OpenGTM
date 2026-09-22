// This blocking head script and React share one store so the first paint,
// portals, and live system changes cannot disagree about the active scheme.
;(function () {
  if (window.OpenGTMTheme) return
  var media = window.matchMedia("(prefers-color-scheme: dark)")
  var listeners = new Set()
  function normalize(value) {
    return value === "light" || value === "system" ? value : "dark"
  }
  function read() {
    try { return normalize(window.localStorage.getItem("theme")) }
    catch { return "dark" }
  }
  var preference = read()
  function scheme() {
    return preference === "system" ? (media.matches ? "dark" : "light") : preference
  }
  function update() {
    var resolved = scheme()
    document.documentElement.classList.toggle("dark", resolved === "dark")
    document.documentElement.classList.toggle("light", resolved === "light")
    document.documentElement.style.colorScheme = resolved
    listeners.forEach(function (listener) { listener() })
  }
  window.OpenGTMTheme = {
    getSnapshot: function () { return preference + ":" + scheme() },
    setPreference: function (value) {
      preference = normalize(value)
      try { window.localStorage.setItem("theme", preference) } catch { /* Private storage can be unavailable. */ }
      update()
    },
    subscribe: function (listener) {
      listeners.add(listener)
      return function () { listeners.delete(listener) }
    },
  }
  media.addEventListener("change", update)
  window.addEventListener("storage", function (event) {
    if (event.key === "theme" || event.key === null) {
      preference = read()
      update()
    }
  })
  update()
})()
