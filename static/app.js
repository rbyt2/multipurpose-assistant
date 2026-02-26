(() => {
  const isInVisualizer = !!document.querySelector('.visualizer-body');

  function createBackendUrl(path) {
    const base = window.location.origin;
    return base.replace(/\/$/, '') + path;
  }

  // Shared WebSocket connection for visualizer
  let ws;
  let chart;

  function initWebSocket() {
    const protocol = window.location.protocol === 'https:' ? 'wss' : 'ws';
    const wsUrl = `${protocol}://${window.location.host}/ws`;

    ws = new WebSocket(wsUrl);

    ws.addEventListener('open', () => {
      console.log('[ws] connected');
    });

    ws.addEventListener('close', () => {
      console.log('[ws] disconnected');
      // Basic reconnect with backoff
      setTimeout(initWebSocket, 2000);
    });

    ws.addEventListener('message', (event) => {
      try {
        const payload = JSON.parse(event.data);
        handleWsMessage(payload);
      } catch (e) {
        console.warn('[ws] non-JSON message', event.data);
      }
    });
  }

  function handleWsMessage(msg) {
    if (!isInVisualizer) return;
    if (!msg || typeof msg !== 'object') return;

    if (msg.type === 'status') {
      updateStatus(msg.state || 'idle');
    } else if (msg.type === 'response') {
      updateResponseText(msg.text || '');
      if (msg.needsChart && msg.chartData) {
        renderChart(msg.chartData);
      }
    }
  }

  function updateStatus(state) {
    const root = document.querySelector('.visualizer-center');
    if (!root) return;
    const statusLabel = document.getElementById('status-label');

    const canonical = (state || 'idle').toLowerCase();
    const states = ['idle', 'listening', 'thinking', 'speaking'];

    states.forEach((s) => root.classList.remove(`state-${s}`));
    const target = states.includes(canonical) ? canonical : 'idle';
    root.classList.add(`state-${target}`);

    if (statusLabel) {
      statusLabel.textContent = target;
    }
  }

  function updateResponseText(text) {
    const el = document.getElementById('response-text-display');
    if (!el) return;
    el.classList.remove('visible');
    // Small timeout so CSS transition re-triggers
    setTimeout(() => {
      el.textContent = text;
      el.classList.add('visible');
    }, 20);
  }

  function renderChart(chartData) {
    const ctx = document.getElementById('assistant-chart');
    if (!ctx || !window.Chart) return;

    const type = chartData.type || 'bar';
    const data = {
      labels: chartData.labels || [],
      datasets: chartData.datasets || [],
    };
    const options = chartData.options || {
      responsive: true,
      maintainAspectRatio: false,
      plugins: {
        legend: {
          labels: {
            color: '#e6e9ff',
            font: { size: 10 },
          },
        },
      },
      scales: {
        x: {
          ticks: { color: '#ccd1ff', font: { size: 9 } },
          grid: { color: 'rgba(180, 187, 255, 0.18)' },
        },
        y: {
          ticks: { color: '#ccd1ff', font: { size: 9 } },
          grid: { color: 'rgba(180, 187, 255, 0.18)' },
        },
      },
    };

    if (chart) {
      chart.destroy();
    }

    chart = new Chart(ctx, { type, data, options });
  }

  function initTextForm() {
    const form = document.getElementById('text-form');
    const input = document.getElementById('text-input');
    const output = document.getElementById('response-text');

    if (!form || !input || !output) return;

    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      const text = input.value.trim();
      if (!text) return;

      output.textContent = 'Thinking...';

      try {
        const res = await fetch(createBackendUrl('/query'), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ text, mode: 'text' }),
        });
        if (!res.ok) {
          output.textContent = `Error: ${res.status}`;
          return;
        }
        const data = await res.json();
        output.textContent = data.response || '';
      } catch (err) {
        console.error('query error', err);
        output.textContent = 'Request failed.';
      }
    });
  }

  document.addEventListener('DOMContentLoaded', () => {
    if (isInVisualizer) {
      initWebSocket();
      updateStatus('idle');
    } else {
      initTextForm();
    }
  });
})();

