// ascii-tv web player. Streams the same frames curl gets, in "plain" format:
// each frame is "@<ms> <slug>\n" + artwork lines, terminated by "\f".
// No dependencies, no cookies, nothing leaves the page except requests to us.
(() => {
  'use strict';

  const $ = (id) => document.getElementById(id);
  const host = document.body.dataset.host || location.host;
  const reduceMotion = matchMedia('(prefers-reduced-motion: reduce)').matches;

  const store = {
    get(k) { try { return localStorage.getItem('ascii-tv:' + k); } catch { return null; } },
    set(k, v) { try { v == null ? localStorage.removeItem('ascii-tv:' + k) : localStorage.setItem('ascii-tv:' + k, v); } catch { /* private mode */ } },
  };

  const el = {
    screen: $('screen'), frame: $('frame'), play: $('play'), seek: $('seek'), clock: $('clock'),
    movie: $('movie'), cc: $('cc'), smaller: $('smaller'), bigger: $('bigger'), full: $('full'),
    captions: $('captions'), now: $('now'), credit: $('credit'), films: $('films'),
  };

  let movies = [];
  let current = null;        // movie object, or {slug:'live'}
  let playing = false;
  let positionMs = 0;        // last frame's timestamp
  let ctrl = null;           // AbortController for the active stream
  let pending = null;        // latest frame waiting for the next paint
  let captionsOn = store.get('cc') !== 'off';
  let scale = parseFloat(store.get('scale') || '1');
  let lastCaption = '';
  let seeking = false;

  const fmt = (s) => {
    s = Math.max(0, Math.floor(s));
    const h = Math.floor(s / 3600), m = Math.floor(s / 60) % 60, sec = s % 60;
    const mm = String(m).padStart(2, '0'), ss = String(sec).padStart(2, '0');
    return h ? `${h}:${mm}:${ss}` : `${mm}:${ss}`;
  };

  const authHeaders = () => {
    const k = store.get('key');
    return k ? { Authorization: 'Bearer ' + k } : {};
  };

  // ---- fitting the picture -------------------------------------------------

  let charRatio = 0.6;
  (() => {
    const c = document.createElement('canvas').getContext('2d');
    const fam = getComputedStyle(el.frame).fontFamily;
    c.font = `100px ${fam}`;
    const w = c.measureText('M'.repeat(10)).width / 10;
    if (w > 0) charRatio = w / 100;
  })();

  function fit() {
    const cols = current && current.width ? current.width : 67;
    const rows = current && current.height ? current.height : 13;
    const box = el.screen.getBoundingClientRect();
    const pad = parseFloat(getComputedStyle(el.screen).paddingLeft) * 2;
    const availW = box.width - pad;
    const availH = document.fullscreenElement ? box.height - pad : Infinity;
    let size = availW / (cols * charRatio);
    size = Math.min(size, availH / (rows * 1.15));
    size = Math.max(4, Math.min(size * scale, 48));
    el.frame.style.fontSize = size.toFixed(2) + 'px';
  }
  new ResizeObserver(fit).observe(el.screen);
  document.addEventListener('fullscreenchange', fit);

  // ---- streaming -----------------------------------------------------------

  function setPlaying(p) {
    playing = p;
    el.play.dataset.state = p ? 'playing' : 'paused';
    el.play.setAttribute('aria-label', p ? 'Pause' : 'Play');
  }

  function stop() {
    if (ctrl) ctrl.abort();
    ctrl = null;
    setPlaying(false);
  }

  async function start(fromSec) {
    stop();
    if (!current) return;
    const live = current.slug === 'live';
    const params = new URLSearchParams({ format: 'plain', bar: '0' });
    if (!live && fromSec > 0) params.set('t', String(Math.floor(fromSec)));
    ctrl = new AbortController();
    const mine = ctrl;
    setPlaying(true);
    try {
      const res = await fetch(`/${current.slug}?${params}`, { headers: authHeaders(), signal: mine.signal, cache: 'no-store' });
      if (!res.ok) {
        showMessage(res.status === 429 ? 'The theatre is full right now.\nTry again in a minute.' : 'That movie is not available.');
        setPlaying(false);
        return;
      }
      const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
      let buf = '';
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += value;
        let cut;
        while ((cut = buf.indexOf('\f')) >= 0) {
          pending = buf.slice(0, cut);
          buf = buf.slice(cut + 1);
        }
        requestAnimationFrame(paint);
      }
      if (mine === ctrl) { setPlaying(false); }
    } catch (e) {
      if (e.name !== 'AbortError') { showMessage('Lost the signal. Press play to try again.'); setPlaying(false); }
    }
  }

  function showMessage(msg) {
    el.frame.textContent = '\n\n' + msg.split('\n').map((l) => '   ' + l).join('\n') + '\n\n';
  }

  function paint() {
    if (pending == null) return;
    const raw = pending;
    pending = null;
    const nl = raw.indexOf('\n');
    const head = raw.slice(0, nl);
    const art = raw.slice(nl + 1).replace(/\n$/, '');
    const [msStr, slug] = head.slice(1).split(' ');
    positionMs = parseInt(msStr, 10) || 0;

    let lines = art.split('\n');
    const m = current && current.slug === 'live' ? movies.find((x) => x.slug === slug) : current;
    if (current && current.slug === 'live' && m && el.now.dataset.slug !== slug) {
      el.now.dataset.slug = slug;
      el.now.textContent = 'Live: ' + m.title;
      el.credit.textContent = m.credit || '';
      current.width = m.width; current.height = m.height; current.seconds = m.seconds;
      fit();
    }
    const capN = m && m.captionLines ? m.captionLines : 0;
    if (capN) {
      const cap = lines.slice(-capN).map((l) => l.trim()).filter(Boolean).join(' ');
      if (!captionsOn) lines = lines.slice(0, -capN);
      if (cap !== lastCaption) {
        lastCaption = cap;
        el.captions.textContent = captionsOn ? cap : '';
      }
    }
    el.frame.textContent = lines.join('\n');
    const total = (m && m.seconds) || 0;
    if (!seeking) el.seek.value = String(Math.floor(positionMs / 1000));
    el.seek.max = String(total);
    el.clock.textContent = `${fmt(positionMs / 1000)} / ${fmt(total)}`;
  }

  // ---- choosing a movie ----------------------------------------------------

  function choose(slug, autoplay) {
    const live = slug === 'live';
    const m = live ? { slug: 'live', title: 'Live channel', width: movies[0]?.width, height: movies[0]?.height } : movies.find((x) => x.slug === slug);
    if (!m) return;
    current = { ...m };
    el.movie.value = slug;
    el.now.textContent = live ? 'Live channel' : m.title + (m.year ? ` (${m.year})` : '');
    el.now.dataset.slug = live ? '' : slug;
    el.credit.textContent = live ? 'Everyone watching sees the same frame.' : (m.credit || '');
    el.frame.setAttribute('aria-label', `Text-art animation: ${live ? 'live channel' : m.title}`);
    el.cc.hidden = !(m.captionLines || live);
    el.seek.disabled = live;
    el.captions.textContent = '';
    lastCaption = '';
    store.set('last', slug);
    updateCommands(live ? 'live' : slug);
    positionMs = 0;
    el.seek.value = '0';
    el.seek.max = String(m.seconds || 0);
    el.clock.textContent = live ? 'Live' : `00:00 / ${fmt(m.seconds || 0)}`;
    fit();
    if (autoplay) start(0); else { stop(); showMessage(`Press play to watch
${live ? 'the live channel' : m.title}.`); }
  }

  function updateCommands(slug) {
    document.querySelectorAll('code[data-template]').forEach((c) => {
      c.textContent = c.dataset.template.replaceAll('{slug}', slug);
    });
  }

  function renderListings() {
    el.movie.replaceChildren();
    el.films.replaceChildren();
    for (const m of movies) {
      el.movie.add(new Option(m.title + (m.private ? ' (private)' : ''), m.slug));
      const li = document.createElement('li');
      li.className = 'film';
      const t = document.createElement('span');
      t.className = 'film-title';
      t.textContent = m.title + (m.year ? ` (${m.year})` : '');
      const rt = document.createElement('span');
      rt.className = 'film-runtime';
      rt.textContent = fmt(m.seconds);
      const b = document.createElement('button');
      b.type = 'button';
      b.textContent = 'Watch here';
      b.addEventListener('click', () => { choose(m.slug, true); el.screen.scrollIntoView({ behavior: reduceMotion ? 'auto' : 'smooth' }); });
      const code = document.createElement('code');
      code.textContent = `curl ${host}/${m.slug}`;
      li.append(t, rt, b, code);
      el.films.append(li);
    }
    el.movie.add(new Option('Live channel', 'live'));
  }

  async function loadMovies() {
    try {
      const res = await fetch('/api/movies', { headers: authHeaders(), cache: 'no-store' });
      const data = await res.json();
      movies = data.movies || [];
      return data.default;
    } catch {
      return null;
    }
  }

  // ---- controls ------------------------------------------------------------

  el.play.addEventListener('click', () => (playing ? stop() : start(positionMs / 1000)));
  el.seek.addEventListener('input', () => {
    seeking = true;
    el.clock.textContent = `${fmt(+el.seek.value)} / ${fmt(current?.seconds || 0)}`;
  });
  el.seek.addEventListener('change', () => { seeking = false; positionMs = +el.seek.value * 1000; start(+el.seek.value); });
  el.movie.addEventListener('change', () => choose(el.movie.value, true));
  el.cc.addEventListener('click', () => {
    captionsOn = !captionsOn;
    store.set('cc', captionsOn ? 'on' : 'off');
    el.cc.setAttribute('aria-pressed', String(captionsOn));
    if (!captionsOn) el.captions.textContent = '';
  });
  el.cc.setAttribute('aria-pressed', String(captionsOn));
  const zoom = (f) => { scale = Math.min(2, Math.max(0.5, scale * f)); store.set('scale', scale.toFixed(2)); fit(); };
  el.smaller.addEventListener('click', () => zoom(1 / 1.15));
  el.bigger.addEventListener('click', () => zoom(1.15));
  el.full.addEventListener('click', () => {
    if (document.fullscreenElement) document.exitFullscreen();
    else el.screen.requestFullscreen?.().catch(() => {});
  });

  document.addEventListener('keydown', (e) => {
    if (e.target.closest('input, select, textarea, [role="tab"], summary') || e.metaKey || e.ctrlKey || e.altKey) return;
    const k = e.key;
    if (k === ' ' || k === 'k') {
      if (e.target.closest('button')) return; // let the focused button handle Space
      e.preventDefault(); el.play.click();
    } else if (k === 'ArrowRight' || k === 'ArrowLeft') {
      if (!current || current.slug === 'live') return;
      e.preventDefault();
      const t = Math.max(0, positionMs / 1000 + (k === 'ArrowRight' ? 10 : -10));
      positionMs = t * 1000; start(t);
    } else if (k === 'f') { el.full.click(); }
    else if (k === 'c') { if (!el.cc.hidden) el.cc.click(); }
    else if (k === '+' || k === '=') { zoom(1.15); }
    else if (k === '-') { zoom(1 / 1.15); }
  });

  // Pause when the tab is hidden: no point streaming to nobody.
  document.addEventListener('visibilitychange', () => {
    if (document.hidden && playing && current?.slug !== 'live') stop();
  });

  // ---- copy buttons --------------------------------------------------------

  document.querySelectorAll('.copy').forEach((b) => {
    b.addEventListener('click', async () => {
      const text = $(b.dataset.copy).textContent.trim();
      try {
        await navigator.clipboard.writeText(text);
        b.textContent = 'Copied';
      } catch {
        const r = document.createRange();
        r.selectNodeContents($(b.dataset.copy));
        const s = getSelection(); s.removeAllRanges(); s.addRange(r);
        b.textContent = 'Selected';
      }
      setTimeout(() => { b.textContent = 'Copy'; }, 1600);
    });
  });

  // ---- OS tabs (accessible tablist with arrow-key navigation) ---------------

  const tabs = [...document.querySelectorAll('[role="tab"]')];
  function selectTab(tab, focus) {
    for (const t of tabs) {
      const on = t === tab;
      t.setAttribute('aria-selected', String(on));
      t.tabIndex = on ? 0 : -1;
      $(t.getAttribute('aria-controls')).hidden = !on;
    }
    if (focus) tab.focus();
  }
  tabs.forEach((t, i) => {
    t.addEventListener('click', () => selectTab(t, false));
    t.addEventListener('keydown', (e) => {
      let j = null;
      if (e.key === 'ArrowRight') j = (i + 1) % tabs.length;
      if (e.key === 'ArrowLeft') j = (i - 1 + tabs.length) % tabs.length;
      if (e.key === 'Home') j = 0;
      if (e.key === 'End') j = tabs.length - 1;
      if (j != null) { e.preventDefault(); selectTab(tabs[j], true); }
    });
  });
  const ua = (navigator.userAgentData?.platform || navigator.platform || navigator.userAgent || '').toLowerCase();
  const mobile = /android|iphone|ipad|ipod/.test(navigator.userAgent.toLowerCase()) || navigator.userAgentData?.mobile;
  const osTab = mobile ? 'tab-phone' : ua.includes('win') ? 'tab-win' : ua.includes('mac') ? 'tab-mac' : ua.includes('linux') ? 'tab-linux' : 'tab-win';
  selectTab($(osTab), false);

  // ---- private screening key ------------------------------------------------

  const keyStatus = $('keystatus');
  $('unlock').addEventListener('click', (e) => {
    e.preventDefault();
    const box = document.querySelector('.keybox');
    box.open = true;
    box.scrollIntoView({ behavior: reduceMotion ? 'auto' : 'smooth', block: 'center' });
    $('key').focus({ preventScroll: true });
  });
  $('keyform').addEventListener('submit', async (e) => {
    e.preventDefault();
    const k = $('key').value.trim();
    if (!k) return;
    store.set('key', k);
    const before = movies.length;
    await loadMovies();
    if (movies.some((m) => m.private)) {
      keyStatus.textContent = `Unlocked ${movies.length - before} private title${movies.length - before === 1 ? '' : 's'}.`;
      renderListings();
      const firstPrivate = movies.find((m) => m.private);
      choose(firstPrivate ? firstPrivate.slug : movies[0].slug, false);
      el.screen.scrollIntoView({ behavior: reduceMotion ? 'auto' : 'smooth' });
    } else {
      store.set('key', null);
      keyStatus.textContent = 'That key did not unlock anything. Check it and try again.';
    }
    $('key').value = '';
  });
  $('forget').addEventListener('click', async () => {
    store.set('key', null);
    await loadMovies();
    renderListings();
    keyStatus.textContent = 'Key removed from this browser.';
    if (current && !movies.some((m) => m.slug === current.slug) && current.slug !== 'live') choose(movies[0].slug, false);
  });

  // ---- boot ------------------------------------------------------------------

  (async () => {
    // Private screening links look like /?key=XXXX&watch=officespace. Save the
    // key in this browser and strip it from the address bar and history.
    const qs = new URLSearchParams(location.search);
    if (qs.get('key')) {
      store.set('key', qs.get('key'));
      qs.delete('key');
      history.replaceState(null, '', location.pathname + (qs.toString() ? '?' + qs : ''));
    }
    updateCommands('starwars');
    const def = await loadMovies();
    if (!movies.length) { showMessage('The projector is off. Try again soon.'); return; }
    renderListings();
    const want = new URLSearchParams(location.search).get('watch') || store.get('last') || def || movies[0].slug;
    const slug = want === 'live' || movies.some((m) => m.slug === want) ? want : movies[0].slug;
    choose(slug, !reduceMotion);
  })();
})();
