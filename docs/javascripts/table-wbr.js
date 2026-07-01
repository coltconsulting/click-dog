// Insert <wbr> break opportunities after separators in table code, so long
// identifiers (e.g. `click_dog_query_log_enrichment_failures_total`) wrap at
// `_` / `.` / `:` instead of at a random character when a cell is too narrow.
// Plain inline code only — never touches highlighted code blocks (which carry
// child <span> elements), and each element is processed at most once.
(function () {
  function addBreaks() {
    document
      .querySelectorAll(".md-typeset table:not([class]) code")
      .forEach(function (el) {
        if (el.childElementCount !== 0 || el.dataset.wbr === "1") return;
        var text = el.textContent;
        if (!/[_.:]/.test(text)) return;
        var frag = document.createDocumentFragment();
        // split after each `_`, `.` or `:` (keep the separator on the left chunk)
        text.split(/(?<=[_.:])/).forEach(function (chunk) {
          frag.appendChild(document.createTextNode(chunk));
          frag.appendChild(document.createElement("wbr"));
        });
        el.textContent = "";
        el.appendChild(frag);
        el.dataset.wbr = "1";
      });
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", addBreaks);
  } else {
    addBreaks();
  }
})();
