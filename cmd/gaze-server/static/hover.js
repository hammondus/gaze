// Hover readouts for the host page graphs. Each <svg> carries its readout
// in data-hover: a JSON list of [x, line, line, ...], where x is the
// crosshair position in viewBox units and the lines are the text to show
// there. This script only finds the nearest entry to the pointer and
// draws it; the server did the rest.
//
// Plain DOM, no dependencies, and nothing here is needed for the graph to
// render: without it the page loses the readout and nothing else.
"use strict";

(function () {
  var NS = "http://www.w3.org/2000/svg";
  var PAD = 4;       // text inset inside the box, viewBox units
  var LINE = 11;     // line height for 9px text
  var WIDTH = 600;   // matches gWidth in graph.go

  function el(name, cls) {
    var e = document.createElementNS(NS, name);
    if (cls) e.setAttribute("class", cls);
    return e;
  }

  // nearest returns the index of the entry whose x is closest to px.
  // Entries are time-ordered, so x is ascending: binary search.
  function nearest(points, px) {
    var lo = 0, hi = points.length - 1;
    while (lo < hi) {
      var mid = (lo + hi) >> 1;
      if (points[mid][0] < px) lo = mid + 1; else hi = mid;
    }
    if (lo > 0 && px - points[lo - 1][0] < points[lo][0] - px) lo--;
    return lo;
  }

  function attach(svg) {
    var points;
    try { points = JSON.parse(svg.dataset.hover); } catch (e) { return; }
    if (!points || !points.length) return;

    var g = el("g");
    g.style.display = "none";
    var cursor = el("line", "cursor");
    cursor.setAttribute("y1", "0");
    cursor.setAttribute("y2", String(svg.viewBox.baseVal.height));
    var bg = el("rect", "readout-bg");
    bg.setAttribute("rx", "2");
    var text = el("text", "readout");
    g.appendChild(cursor);
    g.appendChild(bg);
    g.appendChild(text);
    svg.appendChild(g);

    var shown = -1;
    function show(i) {
      if (i === shown) return;
      shown = i;
      var x = points[i][0], lines = points[i].slice(1);
      cursor.setAttribute("x1", x);
      cursor.setAttribute("x2", x);

      // Rebuild the lines, then size the box from what they measure.
      while (text.firstChild) text.removeChild(text.firstChild);
      for (var k = 0; k < lines.length; k++) {
        var span = el("tspan");
        span.setAttribute("x", "0");
        span.setAttribute("dy", k === 0 ? "0" : String(LINE));
        span.textContent = lines[k];
        text.appendChild(span);
      }
      var box = text.getBBox();
      // Readout to the right of the cursor, flipped left near the edge.
      var right = x + 6 + box.width + 2 * PAD <= WIDTH - 2;
      var x0 = right ? x + 6 : x - 6 - box.width - 2 * PAD;
      var y0 = 4;
      text.setAttribute("transform", "translate(" + (x0 + PAD) + "," + (y0 + PAD + 8) + ")");
      bg.setAttribute("x", x0);
      bg.setAttribute("y", y0);
      bg.setAttribute("width", box.width + 2 * PAD);
      bg.setAttribute("height", box.height + 2 * PAD);
    }

    // Pointer position in viewBox units, through the SVG's own transform,
    // so the same code works at any rendered size.
    var pt = svg.createSVGPoint();
    svg.addEventListener("pointermove", function (e) {
      pt.x = e.clientX;
      pt.y = e.clientY;
      var ctm = svg.getScreenCTM();
      if (!ctm) return;
      var local = pt.matrixTransform(ctm.inverse());
      g.style.display = "";
      show(nearest(points, local.x));
    });
    svg.addEventListener("pointerleave", function () {
      g.style.display = "none";
      shown = -1;
    });
  }

  var svgs = document.querySelectorAll(".graph svg[data-hover]");
  for (var i = 0; i < svgs.length; i++) attach(svgs[i]);
})();
