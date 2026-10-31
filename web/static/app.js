// KR Host - minimal vanilla JS: shell, wizard, password helpers, copy, confirms, metric polling.
(function () {
  // Sidebar: mobile toggle + active nav state.
  var sidebar = document.getElementById("sidebar");
  var scrim = document.querySelector("[data-sidebar-scrim]");
  document.querySelectorAll("[data-sidebar-toggle]").forEach(function (b) {
    b.addEventListener("click", function () {
      if (sidebar) sidebar.classList.toggle("open");
      if (scrim) scrim.classList.toggle("show");
    });
  });
  if (scrim) scrim.addEventListener("click", function () {
    if (sidebar) sidebar.classList.remove("open");
    scrim.classList.remove("show");
  });
  var path = location.pathname;
  var alias = { "/": "/dashboard", "/jobs": "/deployments", "/monitoring": "/server", "/audit": "/audit", "/account": "/account" };
  document.querySelectorAll("[data-nav]").forEach(function (a) {
    var key = a.getAttribute("data-nav");
    var hit = path === key || path.indexOf(key + "/") === 0 ||
      (alias[path] === key) || (key === "/deployments" && path.indexOf("/jobs") === 0) ||
      (key === "/server" && path === "/monitoring") ||
      (key === "/audit" && (path === "/audit" || path === "/audit-logs")) ||
      (key === "/account" && (path === "/account" || path === "/settings/security"));
    if (key === "/server/containers" && path === "/server/containers") hit = true;
    if (key === "/server" && path === "/server/containers") hit = false;
    if (hit) a.classList.add("active");
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
      document.querySelectorAll(".steps li").forEach(function (li, i) {
        li.classList.toggle("current", i === Number(n) - 1);
        li.classList.toggle("done", i < Number(n) - 1);
      });
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
  // Live logs: poll the plain-text API and refresh the viewer.
  var logView = document.getElementById("log-view");
  if (logView) {
    var site = logView.getAttribute("data-site");
    var follow = document.getElementById("log-follow");
    var updated = document.getElementById("log-updated");
    var pollLogs = function () {
      if (document.hidden) return;
      fetch("/api/logs?site=" + encodeURIComponent(site), { credentials: "same-origin" })
        .then(function (r) { return r.text(); })
        .then(function (txt) {
          if (txt !== logView.textContent) {
            logView.textContent = txt;
            if (follow && follow.checked) logView.scrollTop = logView.scrollHeight;
          }
          if (updated) updated.textContent = "Updated " + new Date().toLocaleTimeString();
        })
        .catch(function () {});
    };
    logView.scrollTop = logView.scrollHeight;
    setInterval(pollLogs, 4000);
  }

  // Live job page: refresh while the job is queued/running.
  if (document.querySelector("[data-job-live]")) {
    setTimeout(function () { if (!document.hidden) location.reload(); }, 5000);
  }

  if (window.htmx) { document.body.addEventListener("htmx:configRequest", function () {}); }
})();
