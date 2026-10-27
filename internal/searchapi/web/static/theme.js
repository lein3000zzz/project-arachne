// Theme: system → light → dark, as on lein3000.dev. The stored choice is applied
// by an inline script in each page's head, before the first paint.
(function () {
  const modes = ["system", "light", "dark"];
  let current = document.documentElement.getAttribute("data-theme") || "system";
  const button = document.getElementById("theme");
  const label = () => { button.textContent = "theme: " + current; };
  label();
  button.addEventListener("click", () => {
    current = modes[(modes.indexOf(current) + 1) % modes.length];
    try { localStorage.setItem("theme", current); } catch (_) {}
    if (current === "system") document.documentElement.removeAttribute("data-theme");
    else document.documentElement.setAttribute("data-theme", current);
    label();
  });
})();
