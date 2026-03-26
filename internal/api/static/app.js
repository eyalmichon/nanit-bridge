(function() {
  var babiesEl = document.getElementById('babies');
  var emptyState = document.getElementById('emptyState');
  var wsDot = document.getElementById('wsDot');
  var wsLabel = document.getElementById('wsLabel');
  var babies = {};
  var players = {};
  var notifSettings = {};
  var ws;
  var reconnectDelay = 1000;

  // ── WebSocket ──────────────────────────────────────────

  function scheduleReconnect() {
    wsDot.classList.remove('connected');
    wsLabel.textContent = 'reconnecting...';
    setTimeout(connect, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 2, 10000);
  }

  function connect() {
    var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    var closing = false;
    try { ws = new WebSocket(proto + '//' + location.host + '/ws'); }
    catch(e) { scheduleReconnect(); return; }

    ws.onopen = function() {
      wsDot.classList.add('connected');
      wsLabel.textContent = 'live';
      reconnectDelay = 1000;
    };
    ws.onerror = function() {
      if (!closing) { closing = true; try { ws.close(); } catch(e) {} scheduleReconnect(); }
    };
    ws.onclose = function() {
      if (!closing) { closing = true; scheduleReconnect(); }
    };
    ws.onmessage = function(e) {
      var msg = JSON.parse(e.data);
      if (msg.type === 'initial') {
        babies = {};
        (msg.babies || []).forEach(function(b) { babies[b.uid] = b; });
        Object.keys(players).forEach(destroyPlayer);
        renderAll();
      } else if (msg.type === 'state_update') {
        var prev = babies[msg.baby.uid];
        babies[msg.baby.uid] = msg.baby;
        updateCard(msg.baby.uid, prev);
      } else if (msg.type === 'log_init') {
        appendLogLines(msg.lines || []);
      } else if (msg.type === 'log') {
        appendLogLine(msg.line);
      }
    };
  }

  // ── Rendering ──────────────────────────────────────────

  function renderAll() {
    var uids = Object.keys(babies);
    if (uids.length === 0) { emptyState.style.display = ''; return; }
    emptyState.style.display = 'none';
    babiesEl.querySelectorAll('.baby-card').forEach(function(el) {
      if (!babies[el.dataset.uid]) { destroyPlayer(el.dataset.uid); el.remove(); }
    });
    uids.forEach(function(uid) { updateCard(uid, null); });
  }

  // ── API helpers ────────────────────────────────────────

  function sendControl(uid, action, value) {
    return fetch('/api/babies/' + uid + '/control', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: action, value: value })
    }).then(function(r) {
      if (!r.ok) throw new Error(r.status + ' ' + r.statusText);
      return r;
    });
  }

  function fetchNotifSettings(uid) {
    fetch('/api/babies/' + uid + '/notification_settings')
      .then(function(r) { return r.json(); })
      .then(function(d) {
        notifSettings[uid] = d.settings || {};
        renderNotifToggles(uid);
      })
      .catch(function(e) { console.error('notif settings error:', e); });
  }

  function toggleNotifSetting(uid, key) {
    var cur = notifSettings[uid] && notifSettings[uid][key];
    var newVal = !cur;
    if (notifSettings[uid]) notifSettings[uid][key] = newVal;
    renderNotifToggles(uid);
    fetch('/api/babies/' + uid + '/notification_settings', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ key: key, enabled: newVal })
    })
    .then(function(r) { return r.json(); })
    .then(function(d) { notifSettings[uid] = d.settings || {}; renderNotifToggles(uid); })
    .catch(function(e) { console.error('toggle notif error:', e); });
  }

  // ── Notification cards ─────────────────────────────────

  function renderNotifToggles(uid) { renderNotifCards(uid); }

  var lastSliderValues = {};

  function renderNotifCards(uid, soundSlider, motionSlider) {
    var el = document.getElementById('notif-cards-' + uid);
    if (!el) return;
    var ns = notifSettings[uid] || {};
    if (soundSlider !== undefined) lastSliderValues[uid] = { sound: soundSlider, motion: motionSlider };
    var sv = lastSliderValues[uid] || { sound: 4, motion: 12 };

    var groups = [
      { key: 'SOUND', label: 'Sound Alerts', sensId: 'ctrl-sound-sens-' + uid, sensMin: 0, sensMax: 7, sensVal: sv.sound },
      { key: 'MOTION', label: 'Motion Alerts', sensId: 'ctrl-motion-sens-' + uid, sensMin: 0, sensMax: 24, sensVal: sv.motion },
      { key: 'CAMERA_CRY_DETECTION', label: 'Cry Detection', sensId: null }
    ];

    var html = '';
    for (var i = 0; i < groups.length; i++) {
      var g = groups[i];
      var on = ns[g.key] === true;
      html += '<div class="notif-item">';
      html += '<div class="notif-head">';
      html += '<span class="ctrl-label">' + g.label + '</span>';
      html += '<button class="toggle ' + (on ? 'on' : '') + '" data-uid="' + uid + '" data-key="' + g.key + '"></button>';
      html += '</div>';
      if (g.sensId) {
        html += '<div class="notif-slider">';
        html += '<span class="sens-label">Less</span>';
        html += '<input type="range" id="' + g.sensId + '" min="' + g.sensMin + '" max="' + g.sensMax + '" value="' + g.sensVal + '">';
        html += '<span class="sens-label">More</span>';
        html += '</div>';
      }
      html += '</div>';
    }

    el.innerHTML = html;

    el.querySelectorAll('.toggle').forEach(function(btn) {
      btn.onclick = function() { toggleNotifSetting(this.dataset.uid, this.dataset.key); };
    });

    var soundSens = document.getElementById('ctrl-sound-sens-' + uid);
    if (soundSens) {
      soundSens.onchange = function() {
        sendControl(uid, 'sound_sensitivity', 9 - parseInt(this.value));
      };
    }
    var motionSens = document.getElementById('ctrl-motion-sens-' + uid);
    if (motionSens) {
      motionSens.onchange = function() {
        sendControl(uid, 'motion_sensitivity', 250000 - (parseInt(this.value) * 10000));
      };
    }
  }

  // ── Pending control state (optimistic UI) ──────────────

  var pendingControls = {};

  function setPending(uid, key, value) {
    if (!pendingControls[uid]) pendingControls[uid] = {};
    pendingControls[uid][key] = { value: value, ts: Date.now() };
  }

  function getPending(uid, key) {
    return pendingControls[uid] && pendingControls[uid][key];
  }

  function clearPending(uid, key) {
    if (pendingControls[uid]) delete pendingControls[uid][key];
  }

  function getControlValue(uid, key, serverValue) {
    var p = getPending(uid, key);
    if (p && (Date.now() - p.ts) < 5000) return p.value;
    if (p) clearPending(uid, key);
    return serverValue;
  }

  // ── Card lifecycle ─────────────────────────────────────

  function ensureCard(uid) {
    var card = babiesEl.querySelector('.baby-card[data-uid="' + uid + '"]');
    if (!card) {
      card = document.createElement('div');
      card.className = 'baby-card';
      card.dataset.uid = uid;
      card.innerHTML =
        '<div class="video-container">' +
          '<video id="video-' + uid + '" muted autoplay playsinline></video>' +
          '<div class="video-overlay" id="overlay-' + uid + '">Stream not active</div>' +
        '</div>' +
        '<div class="baby-body">' +
          '<div id="info-' + uid + '"></div>' +
          '<div id="controls-' + uid + '"></div>' +
        '</div>';
      babiesEl.appendChild(card);
      fetchNotifSettings(uid);
    }
    return card;
  }

  function updateCard(uid, prev) {
    var b = babies[uid];
    if (!b) return;
    emptyState.style.display = 'none';

    var card = ensureCard(uid);
    var infoEl = document.getElementById('info-' + uid);
    var controlsEl = document.getElementById('controls-' + uid);
    var overlay = document.getElementById('overlay-' + uid);

    var s = b.sensors || {};
    var c = b.controls || {};

    var nlOn = getControlValue(uid, 'night_light', c.night_light);
    var nlBright = c.night_light_brightness || 0;
    var nlTimeout = c.night_light_timeout || 0;
    var pbOn = getControlValue(uid, 'playback', c.playback);
    var vol = getControlValue(uid, 'volume', c.volume || 0);
    var br = c.breathing || {};
    var brActive = br.active || false;
    var brCalibrating = br.calibrating || false;
    var brBpm = br.breaths_per_min || 0;
    var brStatusText = !brActive ? 'Off'
      : brCalibrating ? 'Searching for motion\u2026'
      : brBpm === 0 ? 'Scanning for breaths\u2026'
      : brBpm + ' breaths/min';

    var streamPill = b.stream === 'active'
      ? '<span class="pill pill-green">stream</span>'
      : b.stream === 'starting'
        ? '<span class="pill pill-amber">starting</span>'
        : '<span class="pill pill-red">no stream</span>';

    var wsPill = b.ws_alive
      ? '<span class="pill pill-blue">ws</span>'
      : '<span class="pill pill-red">ws off</span>';

    var pushPill = b.push_active
      ? '<span class="pill pill-green">push</span>'
      : '<span class="pill pill-amber">polling</span>';

    var lastUpdate = s.last_update && s.last_update !== '0001-01-01T00:00:00Z'
      ? new Date(s.last_update).toLocaleTimeString() : 'no data yet';

    // ── Info section (header + environment) ──
    infoEl.innerHTML =
      '<div class="baby-header">' +
        '<div><h2>' + esc(b.name || uid) + '</h2>' +
        '<div class="uid">' + esc(uid) + '</div></div>' +
        '<div class="status-pills">' + streamPill + wsPill + pushPill + '</div>' +
      '</div>' +
      '<div class="env-grid">' +
        envCell('Temp', fmtNum(s.temperature, 1), '\u00b0C') +
        envCell('Humidity', fmtNum(s.humidity, 1), '%') +
        envCell('Light', fmtNum(s.light, 0), 'lx') +
      '</div>' +
      '<div class="env-extra">' +
        '<span><span class="env-dot ' + (s.is_night ? 'night' : 'day') + '"></span>' +
          (s.is_night ? 'Nighttime' : 'Daytime') + '</span>' +
        '<span>' + lastUpdate + '</span>' +
      '</div>';

    // ── Controls section ──
    var tracks = c.soundtracks || [];
    var curTrack = c.current_track || '';
    var pollSec = b.sensor_poll_sec || 30;
    var soundSensRaw = c.sound_sensitivity || 5;
    var motionSensRaw = c.motion_sensitivity || 130000;
    var soundSlider = Math.max(0, Math.min(7, 9 - soundSensRaw));
    var motionSlider = Math.max(0, Math.min(24, Math.round((250000 - motionSensRaw) / 10000)));

    var cam = b.camera || {};

    var controlsChanged = !prev || !prev.controls ||
      prev.controls.night_light !== c.night_light ||
      prev.controls.playback !== c.playback ||
      prev.controls.volume !== c.volume ||
      prev.controls.current_track !== c.current_track ||
      (prev.controls.soundtracks || []).length !== tracks.length;

    if (!controlsEl.hasChildNodes() || controlsChanged) {
      renderControlsFull(uid, controlsEl, {
        nlOn: nlOn, nlBright: nlBright, nlTimeout: nlTimeout,
        pbOn: pbOn, vol: vol, tracks: tracks, curTrack: curTrack,
        brActive: brActive, brCalibrating: brCalibrating, brBpm: brBpm,
        brStatusText: brStatusText, pollSec: pollSec,
        s: s, soundSlider: soundSlider, motionSlider: motionSlider,
        cam: cam
      });
    } else {
      syncControls(uid, {
        nlOn: nlOn, nlBright: nlBright, nlTimeout: nlTimeout,
        pbOn: pbOn, curTrack: curTrack,
        brActive: brActive, brCalibrating: brCalibrating, brBpm: brBpm,
        brStatusText: brStatusText,
        soundSlider: soundSlider, motionSlider: motionSlider,
        s: s, cam: cam
      });
    }

    // ── Video stream management ──
    var isActive = b.rtmp_active || b.stream === 'active';
    if (isActive && !players[uid]) {
      overlay.classList.add('hidden');
      startPlayer(uid);
    } else if (!isActive && players[uid]) {
      overlay.textContent = 'Stream not active';
      overlay.classList.remove('hidden');
      destroyPlayer(uid);
    } else if (!isActive) {
      overlay.textContent = 'Stream not active';
      overlay.classList.remove('hidden');
    }
  }

  // ── Full controls render (initial + structural changes) ──

  function renderControlsFull(uid, controlsEl, d) {
    var trackOptions = '';
    for (var i = 0; i < d.tracks.length; i++) {
      var tName = d.tracks[i].name || '';
      var displayName = tName.replace(/\.wav$/i, '');
      var selected = tName === d.curTrack ? ' selected' : '';
      trackOptions += '<option value="' + esc(tName) + '"' + selected + '>' + esc(displayName) + '</option>';
    }
    var trackHtml = d.tracks.length > 0
      ? '<select class="ctrl-select" id="ctrl-track-' + uid + '">' + trackOptions + '</select>'
      : '<span class="ctrl-label">no tracks</span>';

    var nlTimerOptions =
      '<option value="0"'    + (d.nlTimeout === 0    ? ' selected' : '') + '>Always on</option>' +
      '<option value="900"'  + (d.nlTimeout === 900  ? ' selected' : '') + '>15 min</option>' +
      '<option value="1800"' + (d.nlTimeout === 1800 ? ' selected' : '') + '>30 min</option>' +
      '<option value="3600"' + (d.nlTimeout === 3600 ? ' selected' : '') + '>1 hour</option>' +
      '<option value="7200"' + (d.nlTimeout === 7200 ? ' selected' : '') + '>2 hours</option>';

    controlsEl.innerHTML =
      '<div class="section-grid">' +

        '<div class="sec">' +
          '<div class="sec-head"><span class="sec-title">Night Light</span>' +
          '<button class="toggle ' + (d.nlOn ? 'on' : '') + '" id="ctrl-nl-' + uid + '"></button></div>' +
          '<div class="ctrl-row">' +
            '<span class="ctrl-label">Brightness</span>' +
            '<div class="slider-row">' +
              '<input type="range" id="ctrl-bright-' + uid + '" min="0" max="100" value="' + d.nlBright + '">' +
              '<span class="slider-val" id="ctrl-bright-val-' + uid + '">' + d.nlBright + '%</span>' +
            '</div>' +
          '</div>' +
          '<div class="ctrl-row">' +
            '<span class="ctrl-label">Timer</span>' +
            '<select class="ctrl-select" id="ctrl-nl-timer-' + uid + '">' + nlTimerOptions + '</select>' +
          '</div>' +
        '</div>' +

        '<div class="sec">' +
          '<div class="sec-head"><span class="sec-title">Sound Machine</span>' +
          '<button class="toggle ' + (d.pbOn ? 'on' : '') + '" id="ctrl-pb-' + uid + '"></button></div>' +
          '<div class="ctrl-row">' +
            '<span class="ctrl-label">Track</span>' +
            trackHtml +
          '</div>' +
          '<div class="ctrl-row">' +
            '<span class="ctrl-label">Volume</span>' +
            '<div class="slider-row">' +
              '<input type="range" id="ctrl-vol-' + uid + '" min="0" max="100" value="' + d.vol + '">' +
              '<span class="slider-val" id="ctrl-vol-val-' + uid + '">' + d.vol + '</span>' +
            '</div>' +
          '</div>' +
        '</div>' +

        '<div class="sec full">' +
          '<div class="sec-head"><span class="sec-title">Alerts &amp; Detection</span></div>' +
          '<div class="alert-grid" id="alert-grid-' + uid + '">' +
            alertChip('Cry', d.s.cry_detected, d.s.cry_detected_at) +
            alertChip('Sound', d.s.sound_alert, d.s.sound_alert_at) +
            alertChip('Motion', d.s.motion_alert, d.s.motion_alert_at) +
          '</div>' +
          '<div id="notif-cards-' + uid + '" style="margin-top:0.4rem;"></div>' +
        '</div>' +

        '<div class="sec">' +
          '<div class="sec-head"><span class="sec-title">Breathing Monitor</span>' +
          '<button class="toggle ' + (d.brActive ? 'on' : '') + '" id="ctrl-br-' + uid + '"></button></div>' +
          '<div id="ctrl-br-status-' + uid + '">' +
            breathingStatusHtml(d.brActive, d.brCalibrating, d.brBpm, d.brStatusText) +
          '</div>' +
        '</div>' +

        '<div class="sec">' +
          '<div class="sec-head"><span class="sec-title">Settings</span></div>' +
          '<div class="ctrl-row">' +
            '<span class="ctrl-label">Sensor Poll</span>' +
            '<div class="slider-row">' +
              '<input type="range" id="ctrl-poll-' + uid + '" min="5" max="120" value="' + d.pollSec + '">' +
              '<span class="slider-val" id="ctrl-poll-val-' + uid + '">' + d.pollSec + 's</span>' +
            '</div>' +
          '</div>' +
        '</div>' +

        '<div class="sec">' +
          '<div class="sec-head"><span class="sec-title">Camera Info</span></div>' +
          '<div id="camera-info-' + uid + '">' +
            cameraInfoHtml(d.cam) +
          '</div>' +
        '</div>' +

      '</div>';

    wireEventHandlers(uid);
    renderNotifCards(uid, d.soundSlider, d.motionSlider);
  }

  // ── Wire event handlers after full render ──

  function wireEventHandlers(uid) {
    var nlBtn = document.getElementById('ctrl-nl-' + uid);
    if (nlBtn) nlBtn.onclick = function() {
      var newVal = !this.classList.contains('on');
      setPending(uid, 'night_light', newVal);
      sendControl(uid, 'night_light', newVal);
      this.classList.toggle('on', newVal);
    };

    var pbBtn = document.getElementById('ctrl-pb-' + uid);
    if (pbBtn) pbBtn.onclick = function() {
      var newVal = !this.classList.contains('on');
      setPending(uid, 'playback', newVal);
      sendControl(uid, 'playback', newVal);
      this.classList.toggle('on', newVal);
    };

    var trackSel = document.getElementById('ctrl-track-' + uid);
    if (trackSel) trackSel.onchange = function() { sendControl(uid, 'select_track', this.value); };

    var brightSlider = document.getElementById('ctrl-bright-' + uid);
    var brightVal = document.getElementById('ctrl-bright-val-' + uid);
    if (brightSlider) {
      brightSlider.oninput = function() { brightVal.textContent = this.value + '%'; };
      brightSlider.onchange = function() { sendControl(uid, 'night_light_brightness', parseInt(this.value)); };
    }

    var nlTimerSel = document.getElementById('ctrl-nl-timer-' + uid);
    if (nlTimerSel) nlTimerSel.onchange = function() { sendControl(uid, 'night_light_timeout', parseInt(this.value)); };

    var volSlider = document.getElementById('ctrl-vol-' + uid);
    var volVal = document.getElementById('ctrl-vol-val-' + uid);
    if (volSlider) {
      volSlider.oninput = function() { volVal.textContent = this.value; };
      volSlider.onchange = function() {
        var v = parseInt(this.value);
        setPending(uid, 'volume', v);
        sendControl(uid, 'volume', v);
      };
    }

    var brBtn = document.getElementById('ctrl-br-' + uid);
    if (brBtn) brBtn.onclick = function() {
      var btn = this;
      var oldVal = btn.classList.contains('on');
      var newVal = !oldVal;
      btn.classList.add('pending');
      btn.classList.toggle('on', newVal);
      var statusEl = document.getElementById('ctrl-br-status-' + uid);
      if (statusEl) statusEl.innerHTML = breathingStatusHtml(newVal, false, 0, newVal ? 'Starting\u2026' : 'Stopping\u2026');
      var revertTimer = setTimeout(function() {
        if (btn.classList.contains('pending')) {
          btn.classList.remove('pending');
          btn.classList.toggle('on', oldVal);
          if (statusEl) statusEl.innerHTML = breathingStatusHtml(oldVal, false, 0, oldVal ? '' : 'Off');
        }
      }, 8000);
      setPending(uid, 'breathing', { timer: revertTimer });
      sendControl(uid, 'breathing_monitoring', newVal).catch(function() {
        clearTimeout(revertTimer);
        btn.classList.remove('pending');
        btn.classList.toggle('on', oldVal);
        if (statusEl) statusEl.innerHTML = breathingStatusHtml(oldVal, false, 0, oldVal ? '' : 'Off');
        clearPending(uid, 'breathing');
      });
    };

    var pollSlider = document.getElementById('ctrl-poll-' + uid);
    var pollVal = document.getElementById('ctrl-poll-val-' + uid);
    if (pollSlider) {
      pollSlider.oninput = function() { pollVal.textContent = this.value + 's'; };
      pollSlider.onchange = function() { sendControl(uid, 'sensor_poll', parseInt(this.value)); };
    }
  }

  // ── Sync-only update (no DOM rebuild) ──

  function syncControls(uid, d) {
    var nlBtnSync = document.getElementById('ctrl-nl-' + uid);
    if (nlBtnSync) nlBtnSync.classList.toggle('on', d.nlOn);

    var brightSync = document.getElementById('ctrl-bright-' + uid);
    var brightValSync = document.getElementById('ctrl-bright-val-' + uid);
    if (brightSync) { brightSync.value = d.nlBright; brightValSync.textContent = d.nlBright + '%'; }

    var nlTimerSync = document.getElementById('ctrl-nl-timer-' + uid);
    if (nlTimerSync) nlTimerSync.value = d.nlTimeout;

    var pbBtnSync = document.getElementById('ctrl-pb-' + uid);
    if (pbBtnSync) pbBtnSync.classList.toggle('on', d.pbOn);

    var trackSync = document.getElementById('ctrl-track-' + uid);
    if (trackSync && d.curTrack) trackSync.value = d.curTrack;

    var soundSensEl = document.getElementById('ctrl-sound-sens-' + uid);
    if (soundSensEl) soundSensEl.value = d.soundSlider;

    var motionSensEl = document.getElementById('ctrl-motion-sens-' + uid);
    if (motionSensEl) motionSensEl.value = d.motionSlider;

    var brBtnSync = document.getElementById('ctrl-br-' + uid);
    if (brBtnSync) {
      var brPend = getPending(uid, 'breathing');
      if (brPend && brPend.timer) { clearTimeout(brPend.timer); clearPending(uid, 'breathing'); }
      brBtnSync.classList.remove('pending');
      brBtnSync.classList.toggle('on', d.brActive);
    }

    var brStatusSync = document.getElementById('ctrl-br-status-' + uid);
    if (brStatusSync) brStatusSync.innerHTML = breathingStatusHtml(d.brActive, d.brCalibrating, d.brBpm, d.brStatusText);

    var alertGrid = document.getElementById('alert-grid-' + uid);
    if (alertGrid) {
      alertGrid.innerHTML =
        alertChip('Cry', d.s.cry_detected, d.s.cry_detected_at) +
        alertChip('Sound', d.s.sound_alert, d.s.sound_alert_at) +
        alertChip('Motion', d.s.motion_alert, d.s.motion_alert_at);
    }

    var camInfo = document.getElementById('camera-info-' + uid);
    if (camInfo) camInfo.innerHTML = cameraInfoHtml(d.cam);
  }

  // ── Video player ───────────────────────────────────────

  function startPlayer(uid) {
    if (!mpegts.isSupported()) { console.warn('mpegts.js not supported'); return; }
    var videoEl = document.getElementById('video-' + uid);
    if (!videoEl) return;
    var player = mpegts.createPlayer({
      type: 'flv', isLive: true,
      url: location.origin + '/api/stream/' + uid
    }, {
      enableWorker: true,
      liveBufferLatencyChasing: true,
      liveBufferLatencyMaxLatency: 1.5,
      liveBufferLatencyMinRemain: 0.3,
      lazyLoadMaxDuration: 1,
      deferLoadAfterSourceOpen: false
    });
    player.attachMediaElement(videoEl);
    player.load();
    player.play();
    player.on(mpegts.Events.ERROR, function() {
      setTimeout(function() {
        destroyPlayer(uid);
        if (babies[uid] && babies[uid].stream === 'active') startPlayer(uid);
      }, 2000);
    });
    players[uid] = player;
  }

  function destroyPlayer(uid) {
    if (players[uid]) {
      try { players[uid].pause(); players[uid].unload(); players[uid].detachMediaElement(); players[uid].destroy(); } catch(e) {}
      delete players[uid];
    }
  }

  // ── HTML helpers ───────────────────────────────────────

  function cameraInfoHtml(cam) {
    if (!cam) cam = {};
    var fw = cam.firmware_version || '--';
    var hw = cam.hardware_version || '--';
    var mount = cam.mounting_mode || '--';
    return '<div class="info-grid">' +
      infoRow('Firmware', fw) +
      infoRow('Hardware', hw) +
      infoRow('Mount', mount) +
    '</div>';
  }

  function infoRow(label, value) {
    return '<div class="ctrl-row"><span class="ctrl-label">' + label + '</span><span class="ctrl-val">' + esc(value) + '</span></div>';
  }

  function breathingStatusHtml(active, calibrating, bpm, statusText) {
    if (!active) return '<span class="br-detail">Off</span>';
    if (calibrating || bpm === 0) return '<span class="br-detail" style="color:var(--amber)">' + statusText + '</span>';
    return '<span class="br-bpm">' + bpm + '</span><span class="br-bpm-unit"> breaths/min</span>';
  }

  function envCell(label, value, unit) {
    return '<div class="env-item">' +
      '<div class="env-label">' + label + '</div>' +
      '<div class="env-val">' + value + '<span class="env-unit">' + unit + '</span></div>' +
    '</div>';
  }

  function alertChip(label, detected, detectedAt) {
    var active = false;
    var ago = '';
    if (detected && detectedAt && detectedAt !== '0001-01-01T00:00:00Z') {
      var elapsed = (Date.now() - new Date(detectedAt).getTime()) / 1000;
      active = elapsed < 60;
      if (elapsed < 10) ago = ' just now';
      else if (elapsed < 60) ago = ' ' + Math.round(elapsed) + 's';
      else if (elapsed < 3600) ago = ' ' + Math.round(elapsed / 60) + 'm';
    }
    return '<div class="alert-chip' + (active ? ' active' : '') + '">' +
      '<span class="dot"></span>' + label + ago + '</div>';
  }

  function fmtNum(v, decimals) {
    if (v == null || v === 0) return '--';
    return Number(v).toFixed(decimals);
  }

  function esc(s) {
    var d = document.createElement('div');
    d.textContent = s || '';
    return d.innerHTML;
  }

  // ── Log panel ──────────────────────────────────────────

  var logContent = document.getElementById('logContent');
  var logBody = document.getElementById('logBody');
  var logPanel = document.getElementById('logPanel');
  var logBadge = document.getElementById('logBadge');
  var logHeader = document.getElementById('logHeader');
  var logLineCount = 0;
  var LOG_MAX = 500;

  logHeader.onclick = function() {
    logPanel.classList.toggle('collapsed');
    if (!logPanel.classList.contains('collapsed')) {
      logBody.scrollTop = logBody.scrollHeight;
    }
  };

  function appendLogLine(line) {
    logLineCount++;
    logContent.textContent += line + '\n';
    // Trim if too many lines
    if (logLineCount > LOG_MAX) {
      var text = logContent.textContent;
      var cut = text.indexOf('\n', text.length - LOG_MAX * 80);
      if (cut > 0) { logContent.textContent = text.substring(cut + 1); }
      logLineCount = LOG_MAX;
    }
    logBadge.textContent = logLineCount + ' lines';
    // Auto-scroll if near bottom
    var atBottom = logBody.scrollHeight - logBody.scrollTop - logBody.clientHeight < 60;
    if (atBottom) logBody.scrollTop = logBody.scrollHeight;
  }

  function appendLogLines(lines) {
    logContent.textContent = '';
    logLineCount = 0;
    for (var i = 0; i < lines.length; i++) {
      logLineCount++;
      logContent.textContent += lines[i] + '\n';
    }
    logBadge.textContent = logLineCount + ' lines';
    logBody.scrollTop = logBody.scrollHeight;
  }

  // ── Boot ───────────────────────────────────────────────

  connect();
})();
