// Grokbot overlay: shows what the grokbot daemon hears and answers, and
// plays the spoken reply. All state comes from the daemon's WebSocket, which
// replays the current session's events when this page connects.
(function () {
  'use strict';

  var WS_URL = 'ws://127.0.0.1:8679';
  var KEY_BACK = 461;

  var panel = document.getElementById('panel');
  var statusEl = document.getElementById('status');
  var transcriptEl = document.getElementById('transcript');
  var actionsEl = document.getElementById('actions');
  var answerEl = document.getElementById('answer');
  var bot = document.getElementById('bot');

  var ws = null;
  var closing = false;
  var mood = 'happy'; // the bot's face while Grok answers; set by "mood"
  var face = '';
  var speaker = 'Grok'; // whose words are shown: Grok, or the user's Grok Bot agent

  // face sets the bot's state (see morph-bot's 39 states).
  function setFace(state) {
    if (state !== face && bot.setState) {
      face = state;
      bot.setState(state);
    }
  }

  function send(msg) {
    if (ws && ws.readyState === 1) {
      ws.send(JSON.stringify(msg));
    }
  }

  function setState(state, text) {
    panel.className = 'panel shown ' + state;
    statusEl.textContent = text;
  }

  function reset() {
    closing = false;
    mood = 'happy';
    speaker = 'Grok';
    transcriptEl.textContent = '';
    actionsEl.innerHTML = '';
    answerEl.textContent = '';
    setBounce(0);
  }

  // While Grok speaks (the daemon plays the audio) the bot bounces with the
  // loudness of the voice.
  var bounce = 0;
  function setBounce(level) {
    bounce += (level - bounce) * 0.6;
    bot.style.transform = bounce > 0.01
      ? 'translateY(' + (-14 * bounce).toFixed(1) + 'px) scale(' + (1 + 0.08 * bounce).toFixed(3) + ')'
      : '';
  }

  function close() {
    if (closing) {
      return;
    }
    closing = true;
    setBounce(0);
    setFace('powering-down');
    setTimeout(function () {
      panel.className = 'panel';
    }, 450);
    setTimeout(function () {
      window.close();
    }, 700);
  }

  function onEvent(ev) {
    switch (ev.event) {
      case 'listening':
        reset();
        setState('listening', 'Listening…');
        setFace('listening');
        break;
      case 'followUp':
        // Grok has answered; the answer stays up while it listens for more.
        setState('listening', 'Listening…');
        setFace('listening');
        break;
      case 'level':
        panel.style.setProperty('--level', String(ev.level || 0));
        break;
      case 'transcribing':
        panel.style.setProperty('--level', '0');
        setState('thinking', 'Thinking…');
        setFace('dictating');
        break;
      case 'message':
        // A message from the user's Grok Bot agent, read out by the daemon.
        // It stays under what the user said.
        if (!ev.append) {
          answerEl.textContent = '';
        }
        speaker = ev.from || 'Grok';
        answerEl.textContent += (ev.append ? '\n\n' : '') + ev.text;
        answerEl.scrollTop = answerEl.scrollHeight;
        mood = 'happy';
        setState('done', speaker);
        setFace(mood);
        break;
      case 'transcript':
        transcriptEl.textContent = '“' + ev.text + '”';
        break;
      case 'thinking':
        setState('thinking', 'Thinking…');
        setFace('thinking');
        break;
      case 'mood':
        mood = ev.mood || 'happy';
        setFace(mood);
        break;
      case 'answerDelta':
        if (panel.className.indexOf('answering') < 0) {
          setState('answering', speaker);
        }
        answerEl.textContent += ev.text;
        answerEl.scrollTop = answerEl.scrollHeight;
        break;
      case 'action':
        var chip = document.createElement('span');
        chip.className = 'chip';
        chip.textContent = ev.text;
        actionsEl.appendChild(chip);
        setFace('working');
        if (bot.playMorph) {
          bot.playMorph('send', { restore: null });
        }
        break;
      case 'answerDone':
        setState('done', speaker);
        setFace(mood);
        break;
      case 'speaking':
        setState('speaking', speaker);
        setFace(mood);
        break;
      case 'peek':
        // Hidden while the daemon takes a screenshot for Grok.
        panel.style.visibility = ev.hidden ? 'hidden' : '';
        break;
      case 'voiceLevel':
        setBounce(ev.level || 0);
        break;
      case 'spoken':
        setBounce(0);
        if (!closing) {
          setState('done', speaker);
        }
        break;
      case 'error':
        setState('error', ev.message);
        setFace(/key/i.test(ev.message) ? 'sad' : 'confused');
        break;
      case 'close':
        close();
        break;
    }
  }

  // The daemon passes a key in the launch params; the socket refuses
  // connections without it. Read it on every connect, since a relaunch
  // (after the daemon restarted) brings a new one.
  function launchParams() {
    try {
      return JSON.parse((window.PalmSystem && window.PalmSystem.launchParams) || '{}') || {};
    } catch (err) {
      return {};
    }
  }

  function connect() {
    var key = launchParams().key;
    if (!key || ws) {
      return; // opened by hand, not by the daemon; or already connected
    }
    ws = new WebSocket(WS_URL + '/?key=' + encodeURIComponent(key));
    ws.onmessage = function (e) {
      try {
        onEvent(JSON.parse(e.data));
      } catch (err) {
        // ignore malformed events
      }
    };
    ws.onclose = function () {
      ws = null;
      if (!closing) {
        setTimeout(connect, 500);
      }
    };
  }

  document.addEventListener('keydown', function (e) {
    if (e.keyCode === KEY_BACK || e.keyCode === 27) {
      e.preventDefault();
      send({ type: 'cancel' });
      close();
    }
  });

  // Launched again while open: the daemon starts a new session and replays it.
  document.addEventListener('webOSRelaunch', function () {
    connect();
    reset();
    setState('listening', 'Listening…');
    setFace('waking');
  });

  // Launched with {"demo": true}: cycle through faces and show the frame
  // rate, to check the animation runs smoothly on the TV.
  function demo() {
    var states = ['listening', 'dictating', 'thinking', 'working', 'happy', 'laughing',
      'curious', 'confused', 'excited', 'proud', 'sad', 'searching', 'powering-down'];
    var i = 0;
    var frames = 0;
    var last = Date.now();
    closing = true; // ignore the daemon's events
    panel.className = 'panel shown speaking';
    transcriptEl.textContent = '“Demo”';
    (function count() {
      frames++;
      requestAnimationFrame(count);
    })();
    setInterval(function () {
      var now = Date.now();
      var fps = Math.round(frames * 1000 / (now - last));
      frames = 0;
      last = now;
      statusEl.textContent = face + ' · ' + fps + ' fps';
      setFace(states[i++ % states.length]);
    }, 3000);
    setFace(states[i++]);
  }

  var params = launchParams();
  connect();
  if (params.demo) {
    demo();
  } else {
    setState('listening', 'Listening…');
    setFace('waking');
  }
})();
