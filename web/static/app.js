// Own hPanel - minimal vanilla JS: theme, wizard, password helpers, copy, confirms, metric polling.
(function () {
  var root = document.documentElement;
  var saved = localStorage.getItem("own-hpanel-theme");
  if (saved) root.setAttribute("data-theme", saved);
  document.querySelectorAll("[data-theme-toggle]").forEach(function (b) {
    b.addEventListener("click", function () {
      var next = root.getAttribute("data-theme") === "dark" ? "light" : "dark";
      root.setAttribute("data-theme", next);
      localStorage.setItem("own-hpanel-theme", next);
    });
  });

  // Confirmations for dangerous / disruptive actions.
  document.querySelectorAll("[data-confirm]").forEach(function (el) {
    el.addEventListener("click", function (e) {
      if (!window.confirm(el.getAttribute("data-confirm"))) e.preventDefault();
    });
  });

  // Wizard navigation + server-validated preview.
  var steps = document.querySelectorAll(".wizard-step");
  if (steps.length) {
    var show = function (n) {
      steps.forEach(function (s) { s.hidden = String(s.getAttribute("data-step")) !== String(n); });
      if (String(n) === "2") loadPreview();
    };
    document.querySelectorAll("[data-next-step]").forEach(function (b) {
      b.addEventListener("click", function () { show(b.getAttribute("data-next-step")); });
    });
    var domain = document.getElementById("f-domain");
    var adminDomain = document.getElementById("f-admindomain");
    if (domain && adminDomain) {
      domain.addEventListener("input", function () {
        if (!adminDomain.dataset.touched) adminDomain.placeholder = "wpadmin." + (domain.value || "example.com");
      });
      adminDomain.addEventListener("input", function () { adminDomain.dataset.touched = "1"; });
    }
    var profile = document.getElementById("f-profile");
    if (profile) profile.addEventListener("change", function () {
      document.getElementById("custom-resources").hidden = profile.value !== "custom";
    });
    function loadPreview() {
      var form = document.getElementById("create-form");
      var box = document.getElementById("preview-box");
      if (!form || !box) return;
      var fd = new FormData(form);
      fetch("/websites/preview", { method: "POST", body: new URLSearchParams(fd), credentials: "same-origin" })
        .then(function (r) { return r.json(); })
        .then(function (d) {
          if (d.error) { box.innerHTML = "<p class='error-box'>" + d.error + "</p>"; return; }
          var dns = (d.dns_records || []).map(function (r) { return "Type: A    Name: " + r[1] + "    Value: " + r[2]; }).join("\n");
          box.innerHTML = "<table class='kv'>" +
            "<tr><th>Domain</th><td>" + d.domain + "</td></tr>" +
            "<tr><th>Admin URL</th><td>" + d.admin_url + "</td></tr>" +
            "<tr><th>Admin alias</th><td>" + (d.admin_domain || "") + " → " + d.admin_domain_target + "</td></tr>" +
            "<tr><th>Database</th><td>" + (d.db_name || "(auto-generated)") + "</td></tr>" +
            "<tr><th>PHP</th><td>" + d.php_version + "</td></tr>" +
            "<tr><th>Resources</th><td>" + d.cpu + " CPU max · " + d.mem_mb + " MB RAM max</td></tr></table>";
          var pre = document.getElementById("dns-records");
          if (pre && dns) pre.textContent = dns;
        })
        .catch(function () { box.innerHTML = "<p class='error-box'>Preview request failed.</p>"; });
    }
  }

  // Password generate / show-hide.
  document.querySelectorAll("[data-generate-pw]").forEach(function (b) {
    b.addEventListener("click", function () {
      var target = document.querySelector(b.getAttribute("data-generate-pw"));
      fetch("/api/generate-password", { credentials: "same-origin" })
        .then(function (r) { return r.text(); })
        .then(function (pw) { if (target) { target.value = pw; target.type = "text"; } });
    });
  });
  document.querySelectorAll("[data-toggle-pw]").forEach(function (b) {
    b.addEventListener("click", function () {
      var target = document.querySelector(b.getAttribute("data-toggle-pw"));
      if (target) target.type = target.type === "password" ? "text" : "password";
    });
  });

  // Copy DNS records.
  document.querySelectorAll("[data-copy]").forEach(function (b) {
    b.addEventListener("click", function () {
      var el = document.querySelector(b.getAttribute("data-copy"));
      if (el && navigator.clipboard) navigator.clipboard.writeText(el.textContent.trim()).then(function () { b.textContent = "Copied ✓"; });
    });
  });

  // Dashboard metric polling (lightweight, 15s).
  var cpuEl = document.getElementById("m-cpu");
  if (cpuEl) {
    setInterval(function () {
      fetch("/api/metrics", { credentials: "same-origin" }).then(function (r) { return r.json(); }).then(function (d) {
        cpuEl.textContent = Math.round(d.cpu_percent) + "%";
        var mem = document.getElementById("m-mem");
        if (mem) mem.textContent = fmtBytes(d.mem_used) + " / " + fmtBytes(d.mem_total);
        var disk = document.getElementById("m-disk");
        if (disk) disk.textContent = fmtBytes(d.disk_used) + " / " + fmtBytes(d.disk_total);
      }).catch(function () {});
    }, 15000);
  }
  function fmtBytes(n) {
    if (!n) return "0 B";
    var u = ["B", "KiB", "MiB", "GiB", "TiB"], i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return n.toFixed(1) + " " + u[i];
  }
  if (window.htmx) { document.body.addEventListener("htmx:configRequest", function () {}); }
})();
