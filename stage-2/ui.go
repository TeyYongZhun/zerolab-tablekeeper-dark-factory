package main

import (
	"net/http"
	"strings"
)

// The browser UI is server-rendered shells plus a small amount of inline
// JavaScript that talks to the JSON API. Nothing is loaded from the network.

const pageShell = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%TITLE% - Tablekeeper</title>
<style>
body{font-family:system-ui,sans-serif;margin:0;color:#222;background:#fafafa}
header{background:#1f3a5f;color:#fff;padding:.6rem 1rem;display:flex;gap:1rem;align-items:center;flex-wrap:wrap}
header a{color:#fff;text-decoration:none;margin-right:.8rem}
header .brand{font-weight:700;margin-right:1.2rem}
#auth-nav{margin-left:auto;display:flex;gap:.6rem;align-items:center}
main{max-width:60rem;margin:1rem auto;padding:0 1rem}
form label{display:inline-block;margin:.3rem .8rem .3rem 0}
input,select,button{font:inherit;padding:.35rem .5rem}
button{cursor:pointer}
table.grid{border-collapse:collapse;margin:1rem 0}
table.grid th,table.grid td{border:1px solid #ccc;padding:.2rem .3rem;text-align:center}
table.grid th.rowhead{text-align:left;white-space:nowrap}
button[data-available="true"]{background:#d7f0d7;border:1px solid #6a6}
button[data-available="false"]{background:#eee;color:#888;border:1px solid #ccc;cursor:not-allowed}
.error{color:#a00;margin:.5rem 0}
.uncertain{color:#7a5200;background:#fff3cd;padding:.5rem;margin:.5rem 0}
.box{border:1px solid #ccc;background:#fff;padding:.8rem;margin:1rem 0}
</style>
</head>
<body>
<header>
<span class="brand">Tablekeeper</span>
<nav><a href="/">Find a table</a><a href="/lookup">Look up reservation</a></nav>
<span id="auth-nav"></span>
</header>
<main>
%BODY%
</main>
<script>
%COMMON%
</script>
<script>
%PAGE%
</script>
</body>
</html>`

const commonJS = `
var TK = {};
TK.get = function (k) { try { return JSON.parse(localStorage.getItem(k)); } catch (e) { return null; } };
TK.set = function (k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch (e) {} };
TK.del = function (k) { try { localStorage.removeItem(k); } catch (e) {} };
TK.session = function () { var s = TK.get('tk_session'); return s && s.token ? s : null; };
TK.$ = function (id) { return document.getElementById(id); };
TK.el = function (tag, attrs, text) {
  var e = document.createElement(tag);
  if (attrs) { for (var k in attrs) { e.setAttribute(k, attrs[k]); } }
  if (text !== undefined && text !== null) { e.textContent = text; }
  return e;
};
TK.uuid = function () {
  if (window.crypto && window.crypto.randomUUID) { return window.crypto.randomUUID(); }
  return 'k' + Date.now().toString(36) + Math.random().toString(36).slice(2) + Math.random().toString(36).slice(2);
};
TK.api = function (method, path, body, opts) {
  opts = opts || {};
  var headers = {};
  var s = TK.session();
  if (s) { headers['Authorization'] = 'Bearer ' + s.token; }
  if (body !== undefined && body !== null) { headers['Content-Type'] = 'application/json'; }
  if (opts.key) { headers['Idempotency-Key'] = opts.key; }
  var ctl = new AbortController();
  var timer = setTimeout(function () { ctl.abort(); }, opts.timeout || 4000);
  if (opts.signal) { opts.signal.addEventListener('abort', function () { ctl.abort(); }); }
  return fetch(path, {
    method: method, headers: headers, signal: ctl.signal,
    body: (body === undefined || body === null) ? undefined : JSON.stringify(body)
  }).then(function (res) {
    return res.text().then(function (t) {
      clearTimeout(timer);
      var d = null;
      try { d = JSON.parse(t); } catch (e) {}
      return { status: res.status, data: d };
    });
  }).catch(function (e) { clearTimeout(timer); throw e; });
};
TK.errMsg = function (r, fallback) {
  return (r && r.data && r.data.error && r.data.error.message) || fallback || 'Something went wrong.';
};
TK.errCode = function (r) { return (r && r.data && r.data.error && r.data.error.code) || ''; };
TK.nav = function () {
  var box = TK.$('auth-nav');
  box.textContent = '';
  var s = TK.session();
  if (s) {
    box.appendChild(TK.el('span', { 'data-testid': 'current-user' }, s.display_name));
    var b = TK.el('button', { type: 'button', 'data-testid': 'logout-button' }, 'Log out');
    b.onclick = function () { TK.del('tk_session'); TK.nav(); };
    box.appendChild(b);
  } else {
    var a = TK.el('a', { href: '/login' }, 'Log in');
    var c = TK.el('a', { href: '/signup' }, 'Sign up');
    box.appendChild(a);
    box.appendChild(c);
  }
};
TK.labels = function (detail, ids) {
  var m = {};
  (detail.tables || []).forEach(function (t) { m[t.id] = t.label; });
  return ids.map(function (i) { return m[i] || i; });
};
TK.nav();
`

const signupBody = `
<h1>Sign up</h1>
<form id="signup-form" novalidate>
<p><label>Email <input type="email" name="email" autocomplete="email" data-testid="signup-email"></label></p>
<p><label>Password <input type="password" name="password" autocomplete="new-password" data-testid="signup-password"></label></p>
<p><label>Display name <input type="text" name="display_name" data-testid="signup-display-name"></label></p>
<p><button type="submit" data-testid="signup-submit">Sign up</button></p>
</form>
<div id="auth-error-box"></div>
<p>Already registered? <a href="/login">Log in</a></p>
`

const signupJS = `
TK.$('signup-form').onsubmit = function (e) {
  e.preventDefault();
  var box = TK.$('auth-error-box');
  box.textContent = '';
  var body = {
    email: document.querySelector('[data-testid="signup-email"]').value.trim(),
    password: document.querySelector('[data-testid="signup-password"]').value,
    display_name: document.querySelector('[data-testid="signup-display-name"]').value.trim()
  };
  TK.api('POST', '/auth/signup', body).then(function (r) {
    if (r.status === 201 && r.data && r.data.token) {
      TK.set('tk_session', { token: r.data.token, user_id: r.data.user_id, display_name: r.data.display_name });
      window.location.href = '/';
    } else {
      box.appendChild(TK.el('p', { 'data-testid': 'auth-error', role: 'alert', 'class': 'error' }, TK.errMsg(r, 'Signup failed.')));
    }
  }, function () {
    box.appendChild(TK.el('p', { 'data-testid': 'auth-error', role: 'alert', 'class': 'error' }, 'Network error. Please try again.'));
  });
};
`

const loginBody = `
<h1>Log in</h1>
<form id="login-form" novalidate>
<p><label>Email <input type="email" name="email" autocomplete="email" data-testid="login-email"></label></p>
<p><label>Password <input type="password" name="password" autocomplete="current-password" data-testid="login-password"></label></p>
<p><button type="submit" data-testid="login-submit">Log in</button></p>
</form>
<div id="auth-error-box"></div>
<p>No account yet? <a href="/signup">Sign up</a></p>
`

const loginJS = `
TK.$('login-form').onsubmit = function (e) {
  e.preventDefault();
  var box = TK.$('auth-error-box');
  box.textContent = '';
  var body = {
    email: document.querySelector('[data-testid="login-email"]').value.trim(),
    password: document.querySelector('[data-testid="login-password"]').value
  };
  TK.api('POST', '/auth/login', body).then(function (r) {
    if (r.status === 200 && r.data && r.data.token) {
      TK.set('tk_session', { token: r.data.token, user_id: r.data.user_id, display_name: r.data.display_name });
      window.location.href = '/';
    } else {
      box.appendChild(TK.el('p', { 'data-testid': 'auth-error', role: 'alert', 'class': 'error' }, TK.errMsg(r, 'Login failed.')));
    }
  }, function () {
    box.appendChild(TK.el('p', { 'data-testid': 'auth-error', role: 'alert', 'class': 'error' }, 'Network error. Please try again.'));
  });
};
`

const indexBody = `
<h1>Find a table</h1>
<form id="search-form" novalidate>
<label>Restaurant <select data-testid="restaurant-select" name="restaurant"></select></label>
<label>Date <input type="text" name="date" placeholder="YYYY-MM-DD" autocomplete="off" data-testid="date-input"></label>
<label>Party size <input type="number" name="party_size" min="1" value="2" data-testid="party-size-input"></label>
<button type="submit" data-testid="search-button">Search</button>
</form>
<p id="search-error" class="error" role="alert" hidden></p>
<div id="results"></div>
<div id="booking"></div>
<div id="confirm"></div>
`

const indexJS = `
(function () {
  var sel$ = document.querySelector('[data-testid="restaurant-select"]');
  var date$ = document.querySelector('[data-testid="date-input"]');
  var party$ = document.querySelector('[data-testid="party-size-input"]');
  var seq = 0, searchCtl = null;
  var choice = null;     // currently chosen table set: {restId, restName, tableIds, startLocal, labels, labelMap}
  var pending = null;    // uncertain booking awaiting retry: {key, body, choice}
  var busy = false;
  var detailCache = {};

  function saveForm() { TK.set('tk_form', { restaurant: sel$.value, date: date$.value, party: party$.value }); }
  [sel$, date$, party$].forEach(function (e) { e.addEventListener('input', saveForm); e.addEventListener('change', saveForm); });

  function searchError(msg) {
    var p = TK.$('search-error');
    p.textContent = msg || '';
    p.hidden = !msg;
  }

  function restName(id) {
    for (var i = 0; i < sel$.options.length; i++) { if (sel$.options[i].value === id) { return sel$.options[i].textContent; } }
    return id;
  }

  function search() {
    var rid = sel$.value, date = date$.value.trim(), party = party$.value.trim();
    saveForm();
    searchError('');
    if (!rid) { searchError('Please choose a restaurant.'); return Promise.resolve(); }
    if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) { searchError('Please enter the date as YYYY-MM-DD.'); return Promise.resolve(); }
    if (!/^[0-9]+$/.test(party) || parseInt(party, 10) < 1) { searchError('Party size must be a whole number of at least 1.'); return Promise.resolve(); }
    var my = ++seq;
    if (searchCtl) { searchCtl.abort(); }
    searchCtl = new AbortController();
    var sig = searchCtl.signal;
    var q = 'restaurant_id=' + encodeURIComponent(rid) + '&date=' + encodeURIComponent(date) + '&party_size=' + encodeURIComponent(party);
    return Promise.all([
      TK.api('GET', '/availability?' + q, null, { signal: sig, timeout: 15000 }),
      TK.api('GET', '/restaurants/' + encodeURIComponent(rid), null, { signal: sig, timeout: 15000 })
    ]).then(function (rs) {
      if (my !== seq) { return; }   // a newer search superseded this one
      var av = rs[0], det = rs[1];
      if (av.status !== 200 || det.status !== 200) { searchError(TK.errMsg(av.status !== 200 ? av : det, 'Search failed.')); return; }
      detailCache[rid] = det.data;
      renderGrid(av.data, det.data);
    }, function () {
      if (my !== seq) { return; }
      searchError('Could not load availability. Please try again.');
    });
  }

  function hhmm(slot) { return slot.starts_at_local.slice(11, 16); }

  function renderGrid(av, det) {
    var box = TK.$('results');
    box.textContent = '';
    if (!av.slots || av.slots.length === 0) {
      box.appendChild(TK.el('p', { 'data-testid': 'no-slots' }, 'No tables are bookable on this day.'));
      return;
    }
    var rows = [];
    (det.tables || []).forEach(function (t) { rows.push({ ids: [t.id], title: 'Table ' + t.label + ' (' + t.capacity + ')' }); });
    (det.combinable || []).forEach(function (c) {
      var ls = TK.labels(det, c);
      var cap = 0;
      c.forEach(function (id) { (det.tables || []).forEach(function (t) { if (t.id === id) { cap += t.capacity; } }); });
      rows.push({ ids: c, title: 'Tables ' + ls.join(' + ') + ' (' + cap + ')' });
    });
    var avail = av.slots.map(function (s) {
      var m = {};
      (s.available_options || []).forEach(function (o) { m[o.table_ids.join('+')] = true; });
      return m;
    });
    var grid = TK.el('div', { 'data-testid': 'availability-grid', role: 'grid' });
    var tbl = TK.el('table', { 'class': 'grid' });
    var head = TK.el('tr');
    head.appendChild(TK.el('th'));
    av.slots.forEach(function (s) { head.appendChild(TK.el('th', null, hhmm(s))); });
    tbl.appendChild(head);
    rows.forEach(function (row) {
      var tr = TK.el('tr');
      tr.appendChild(TK.el('th', { 'class': 'rowhead', scope: 'row' }, row.title));
      av.slots.forEach(function (s, i) {
        var key = row.ids.join('+');
        var ok = !!avail[i][key];
        var td = TK.el('td');
        var b = TK.el('button', {
          type: 'button', 'data-testid': 'slot-' + key + '-' + hhmm(s),
          'data-available': ok ? 'true' : 'false', 'aria-disabled': ok ? 'false' : 'true'
        }, ok ? hhmm(s) : '–');
        b.onclick = function () { if (ok) { pick(det, row.ids, s.starts_at_local); } };
        td.appendChild(b);
        tr.appendChild(td);
      });
      tbl.appendChild(tr);
    });
    grid.appendChild(tbl);
    box.appendChild(grid);
  }

  function summary(c) {
    return 'Table ' + c.labels.join(' + ') + ' – ' + c.restName + ' – ' + c.startLocal.replace('T', ' ');
  }

  function pick(det, ids, startLocal) {
    clearPending();
    TK.$('confirm').textContent = '';
    var labelMap = {};
    (det.tables || []).forEach(function (t) { labelMap[t.id] = t.label; });
    choice = { restId: det.id, restName: det.name, tableIds: ids, startLocal: startLocal, labels: TK.labels(det, ids), labelMap: labelMap };
    renderBooking(party$.value.trim(), false);
  }

  function msgBox() { return TK.$('booking-msg'); }
  function clearMsg() { if (msgBox()) { msgBox().textContent = ''; } }
  function showError(text) {
    clearMsg();
    msgBox().appendChild(TK.el('p', { 'data-testid': 'booking-error', role: 'alert', 'class': 'error' }, text));
  }
  function showUncertain() {
    clearMsg();
    msgBox().appendChild(TK.el('p', { 'data-testid': 'booking-uncertain', role: 'status', 'class': 'uncertain' },
      'We did not receive a response, so we cannot tell whether your booking was made. Press Book to retry safely; you will not be booked twice.'));
    var inp = document.querySelector('[data-testid="booking-party-size"]');
    if (inp) { inp.readOnly = true; }
  }

  function renderBooking(party, uncertain) {
    var box = TK.$('booking');
    box.textContent = '';
    var form = TK.el('form', { 'data-testid': 'booking-form', 'class': 'box', novalidate: '' });
    form.appendChild(TK.el('p', { 'data-testid': 'booking-summary' }, summary(choice)));
    var lab = TK.el('label', null, 'Party size ');
    var inp = TK.el('input', { type: 'number', min: '1', 'data-testid': 'booking-party-size' });
    inp.value = party;
    lab.appendChild(inp);
    form.appendChild(lab);
    form.appendChild(TK.el('button', { type: 'submit', 'data-testid': 'booking-submit' }, 'Book'));
    form.appendChild(TK.el('div', { id: 'booking-msg' }));
    form.onsubmit = function (e) { e.preventDefault(); submitBooking(); };
    box.appendChild(form);
    if (uncertain) { showUncertain(); }
  }

  function clearPending() { pending = null; TK.del('tk_pending'); }

  function markUncertain(key, body) {
    pending = { key: key, body: body, choice: choice };
    TK.set('tk_pending', pending);
    showUncertain();
  }

  function submitBooking() {
    if (busy || !choice) { return; }
    var btn = document.querySelector('[data-testid="booking-submit"]');
    var key, body;
    if (pending) {
      key = pending.key; body = pending.body;
    } else {
      var party = document.querySelector('[data-testid="booking-party-size"]').value.trim();
      if (!/^[0-9]+$/.test(party) || parseInt(party, 10) < 1) { showError('Party size must be a whole number of at least 1.'); return; }
      if (!TK.session()) { showError('Please log in to book a table.'); return; }
      body = { restaurant_id: choice.restId, table_ids: choice.tableIds, starts_at_local: choice.startLocal, party_size: parseInt(party, 10) };
      key = TK.uuid();
    }
    if (!TK.session()) { showError('Please log in to book a table.'); return; }
    busy = true;
    btn.disabled = true;
    clearMsg();
    if (pending) { showUncertain(); }
    TK.api('POST', '/reservations', body, { key: key, timeout: 4000 }).then(function (r) {
      busy = false;
      btn.disabled = false;
      if (r.status === 201 || r.status === 200) { success(r.data); return; }
      if (r.status >= 500) { markUncertain(key, body); return; }
      clearPending();
      var inp = document.querySelector('[data-testid="booking-party-size"]');
      if (inp) { inp.readOnly = false; }
      if (r.status === 401) {
        TK.del('tk_session'); TK.nav();
        showError('Your session has expired. Please log in again.');
        return;
      }
      showError(TK.errMsg(r, 'Booking failed.'));
      if (TK.errCode(r) === 'table_unavailable') { search(); }
    }, function () {
      busy = false;
      btn.disabled = false;
      markUncertain(key, body);
    });
  }

  function success(res) {
    clearPending();
    var c = choice;
    TK.$('booking').textContent = '';
    var labels = (res.table_ids || c.tableIds).map(function (id) { return c.labelMap[id] || id; });
    var box = TK.$('confirm');
    box.textContent = '';
    var conf = TK.el('div', { 'data-testid': 'confirmation', 'class': 'box' });
    conf.appendChild(TK.el('h2', null, 'Booking confirmed'));
    var p = TK.el('p', null, 'Your reference: ');
    p.appendChild(TK.el('strong', { 'data-testid': 'confirmation-reference' }, res.reference));
    conf.appendChild(p);
    conf.appendChild(TK.el('p', { 'data-testid': 'confirmation-details' },
      c.restName + ', table ' + labels.join(' + ') + ', ' + res.starts_at_local.replace('T', ' ') + ', party of ' + res.party_size));
    var t = TK.el('p', null, 'Tables: ');
    t.appendChild(TK.el('span', { 'data-testid': 'confirmation-tables' }, labels.join(' + ')));
    conf.appendChild(t);
    box.appendChild(conf);
    choice = null;
    search();
  }

  function init() {
    TK.api('GET', '/restaurants', null, { timeout: 15000 }).then(function (r) {
      var list = (r.data && r.data.restaurants) || [];
      list.forEach(function (x) { sel$.appendChild(TK.el('option', { value: x.id }, x.name)); });
      var f = TK.get('tk_form');
      if (f) {
        if (f.restaurant) { sel$.value = f.restaurant; }
        date$.value = f.date || '';
        if (f.party) { party$.value = f.party; }
      }
      var p = TK.get('tk_pending');
      if (p && p.key && p.body && p.choice) {
        pending = p;
        choice = p.choice;
        renderBooking(String(p.body.party_size), true);
      }
    }, function () { searchError('Could not load restaurants.'); });
  }

  TK.$('search-form').onsubmit = function (e) { e.preventDefault(); search(); };
  init();
})();
`

const lookupBody = `
<h1>Look up a reservation</h1>
<form id="lookup-form" novalidate>
<label>Reference <input type="text" name="reference" autocomplete="off" data-testid="lookup-reference-input"></label>
<button type="submit" data-testid="lookup-submit">Look up</button>
</form>
<div id="lookup-error-box"></div>
<div id="lookup-result"></div>
`

const lookupJS = `
(function () {
  var input$ = document.querySelector('[data-testid="lookup-reference-input"]');
  var seq = 0;

  function showError(text) {
    var box = TK.$('lookup-error-box');
    box.textContent = '';
    if (text) { box.appendChild(TK.el('p', { 'data-testid': 'reservation-error', role: 'alert', 'class': 'error' }, text)); }
  }

  function render(res, det) {
    var out = TK.$('lookup-result');
    out.textContent = '';
    var labels = TK.labels(det, res.table_ids || []);
    var d = TK.el('div', { 'data-testid': 'reservation-detail', 'class': 'box' });
    d.appendChild(TK.el('h2', null, 'Reservation ' + res.reference));
    var st = TK.el('p', null, 'Status: ');
    st.appendChild(TK.el('span', { 'data-testid': 'reservation-status' }, res.status));
    d.appendChild(st);
    d.appendChild(TK.el('p', null, det.name + ', ' + res.starts_at_local.replace('T', ' ') + ', party of ' + res.party_size));
    var t = TK.el('p', null, 'Tables: ');
    t.appendChild(TK.el('span', { 'data-testid': 'reservation-tables' }, labels.join(' + ')));
    d.appendChild(t);
    if (res.status === 'confirmed') {
      var b = TK.el('button', { type: 'button', 'data-testid': 'reservation-cancel-button' }, 'Cancel reservation');
      b.onclick = function () {
        b.disabled = true;
        showError('');
        TK.api('POST', '/reservations/' + encodeURIComponent(res.reference) + '/cancel', null, { timeout: 8000 }).then(function (r) {
          if (r.status === 200) { render(r.data, det); return; }
          b.disabled = false;
          showError(TK.errMsg(r, 'Could not cancel the reservation.'));
        }, function () {
          b.disabled = false;
          showError('Network error. Please try again.');
        });
      };
      d.appendChild(b);
    }
    out.appendChild(d);
  }

  TK.$('lookup-form').onsubmit = function (e) {
    e.preventDefault();
    var ref = input$.value.trim();
    var my = ++seq;
    showError('');
    TK.$('lookup-result').textContent = '';
    if (!ref) { showError('Please enter a reservation reference.'); return; }
    if (!TK.session()) { showError('Please log in to look up a reservation.'); return; }
    TK.api('GET', '/reservations/' + encodeURIComponent(ref), null, { timeout: 8000 }).then(function (r) {
      if (my !== seq) { return; }
      if (r.status === 401) { TK.del('tk_session'); TK.nav(); showError('Your session has expired. Please log in again.'); return; }
      if (r.status !== 200) { showError(r.status === 404 ? 'Reservation not found.' : TK.errMsg(r, 'Lookup failed.')); return; }
      return TK.api('GET', '/restaurants/' + encodeURIComponent(r.data.restaurant_id), null, { timeout: 8000 }).then(function (d) {
        if (my !== seq) { return; }
        render(r.data, d.status === 200 ? d.data : { name: r.data.restaurant_id, tables: [] });
      });
    }, function () { if (my === seq) { showError('Network error. Please try again.'); } });
  };
})();
`

func renderPage(title, body, js string) string {
	r := strings.NewReplacer("%TITLE%", title, "%BODY%", body, "%COMMON%", commonJS, "%PAGE%", js)
	return r.Replace(pageShell)
}

func htmlHandler(title, body, js string) http.HandlerFunc {
	page := renderPage(title, body, js)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(page))
	}
}

func registerUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", htmlHandler("Find a table", indexBody, indexJS))
	mux.HandleFunc("GET /signup", htmlHandler("Sign up", signupBody, signupJS))
	mux.HandleFunc("GET /login", htmlHandler("Log in", loginBody, loginJS))
	mux.HandleFunc("GET /lookup", htmlHandler("Look up reservation", lookupBody, lookupJS))
}
