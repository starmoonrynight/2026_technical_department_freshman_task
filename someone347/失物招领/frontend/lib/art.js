import { esc } from "./utils.js";
export function art(kind = "bag") {
  const shapes = {
    card: '<g transform="rotate(-12 160 100)"><rect x="116" y="32" width="89" height="131" rx="12" fill="#394755"/><rect x="127" y="48" width="66" height="84" rx="5" fill="#e0e9ed"/><path d="M127 57h66" stroke="#8cabc2" stroke-width="14"/><rect x="137" y="79" width="20" height="24" rx="3" fill="#abbecb"/><path d="M164 84h20m-20 8h17m-47 20h45" stroke="#b3c5d0" stroke-width="3"/><circle cx="160" cy="146" r="4" fill="#687888"/></g>',
    earbuds:
      '<ellipse cx="160" cy="142" rx="74" ry="12" fill="#d7dfdd"/><rect x="112" y="83" width="98" height="65" rx="24" fill="#fff" stroke="#d6dedb" stroke-width="2"/><path d="M112 104h98" stroke="#dde5e2" stroke-width="2"/><circle cx="161" cy="124" r="2" fill="#b6cfc3"/><g fill="#fff" stroke="#d2ddd7" stroke-width="2"><rect x="122" y="42" width="13" height="39" rx="6" transform="rotate(-20 125 50)"/><ellipse cx="123" cy="42" rx="14" ry="11"/><rect x="187" y="40" width="13" height="39" rx="6" transform="rotate(20 190 50)"/><ellipse cx="193" cy="40" rx="14" ry="11"/></g>',
    keys: '<path d="M124 53c-6-36 65-40 48-5l-9 40" fill="none" stroke="#729dcc" stroke-width="12" stroke-linecap="round"/><circle cx="153" cy="99" r="23" fill="none" stroke="#a9b1b8" stroke-width="6"/><g transform="rotate(25 145 110)"><circle cx="130" cy="117" r="14" fill="#d3b576"/><circle cx="130" cy="117" r="6" fill="#eee9dc"/><path d="M130 130v38m0-10h11m-11-9h8" stroke="#d3b576" stroke-width="8"/></g><g transform="rotate(-18 163 112)"><circle cx="176" cy="117" r="14" fill="#b9c2cb"/><circle cx="176" cy="117" r="6" fill="#e6e9ed"/><path d="M176 131v38m0-9h10m-10-10h7" stroke="#b9c2cb" stroke-width="8"/></g>',
    book: '<g transform="rotate(-11 160 100)"><rect x="109" y="39" width="105" height="134" rx="5" fill="#c5d5c8"/><path d="M119 40v133" stroke="#87a28e" stroke-width="4"/><rect x="126" y="58" width="75" height="46" rx="2" fill="#e6eee4"/><path d="M138 76h50m-50 10h35m-34 35h48m-43 12h38" stroke="#94ac99" stroke-width="4"/><path d="M115 169h98" stroke="#f4f5ed" stroke-width="5"/></g>',
    bag: '<path d="M131 77V61a29 29 0 0 1 58 0v16" fill="none" stroke="#c8bba4" stroke-width="10"/><path d="m108 77-6 94h116l-8-94z" fill="#e4dbc9"/><path d="M117 82v83m85-83v83" stroke="#d4c8b3" stroke-width="3"/><path d="m145 113 15-10 15 10-15 20z" fill="#bdc8b0"/><path d="M145 145h31" stroke="#baa992" stroke-width="2"/>',
    bottle:
      '<rect x="133" y="35" width="53" height="17" rx="7" fill="#587fa5"/><rect x="126" y="48" width="66" height="123" rx="20" fill="#8db2ce"/><rect x="134" y="67" width="8" height="79" rx="4" fill="#a7c7dd"/><circle cx="162" cy="112" r="19" fill="#e2edf4"/><path d="m150 108 2-9 7 5 7-5 5 9v12h-21z" fill="#a7bccc"/>',
    umbrella:
      '<g transform="rotate(22 160 100)"><path d="M150 29h22l7 100h-37z" fill="#495361"/><path d="M161 130v29c0 19 24 19 24 0" fill="none" stroke="#707b8a" stroke-width="7"/><path d="M141 115h39" stroke="#858f9a" stroke-width="6"/><path d="M153 30v80m10-80v80" stroke="#626e7d" stroke-width="2"/></g>',
  };
  const colors = {
    card: "#e9edf1",
    earbuds: "#e8efeb",
    keys: "#f1ede4",
    book: "#eef0e8",
    bag: "#f3eee6",
    bottle: "#e7eef6",
    umbrella: "#e9edf3",
  };
  return `<svg class="art-svg" viewBox="0 0 320 200" preserveAspectRatio="xMidYMid meet" aria-label="物品示意图" role="img"><rect width="320" height="200" fill="${colors[kind] || "#f0f3f8"}"/><ellipse cx="162" cy="177" rx="71" ry="8" fill="#728297" opacity=".08"/>${shapes[kind] || shapes.bag}</svg>`;
}
export const imageFor = (p) =>
  p.cover_url
    ? `<img src="${esc(p.cover_url)}" alt="${esc(p.title)}" loading="lazy">`
    : art(
        p.illustration ||
          { CARD: "card", DIGITAL: "earbuds", KEY: "keys", BOOK: "book" }[
            p.category
          ] ||
          "bag",
      );
