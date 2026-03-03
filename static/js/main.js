document.addEventListener("DOMContentLoaded", () => {
  const theme = localStorage.getItem("theme");
  if (theme === "dark") document.documentElement.classList.add("dark");
  else if (theme === "light") document.documentElement.classList.remove("dark");
  else {
    const darkMode = window.matchMedia("(prefers-color-scheme: dark)");
    const update = (b) => {
      if (localStorage.getItem("theme") !== null) return;
      if (b) document.documentElement.classList.add("dark");
      else document.documentElement.classList.remove("dark");
    };
    update(darkMode.matches);
    darkMode.addEventListener("change", (e) => update(e.matches));
  }
});

function toggleTheme() {
  const isDarkMode = document.documentElement.classList.contains("dark");
  localStorage.setItem("theme", isDarkMode ? "light" : "dark");
  document.documentElement.classList.toggle("dark");
}
