// Safe localStorage wrapper to prevent crash if storage is disabled/blocked in sandboxed browser environments
const safeStorage = {
    getItem(key) {
        try {
            return localStorage.getItem(key);
        } catch (e) {
            return null;
        }
    },
    setItem(key, val) {
        try {
            localStorage.setItem(key, val);
        } catch (e) {}
    }
};

const proposalsFeed = document.getElementById('proposals-feed');
const feedEmpty = document.getElementById('feed-empty');
const statusTag = document.getElementById('status-tag');
const themeSelect = document.getElementById('theme-select');

const themeClasses = {
    'modern': '',
    'bladerunner': 'theme-bladerunner',
    'synth84': 'theme-synth84',
    'royal': 'theme-royal'
};

function applyTheme(themeKey) {
    // Clear current theme classes
    document.body.classList.remove('theme-bladerunner', 'theme-synth84', 'theme-royal');
    
    // Apply new classes
    const className = themeClasses[themeKey];
    if (className) {
        document.body.classList.add(className);
    }
    
    // Sync dropdown value
    themeSelect.value = themeKey;
    
    safeStorage.setItem('sysThemeKey', themeKey);
    
    // Redraw chart to pick up theme colors immediately
    if (typeof drawLatencyChart === 'function') {
        drawLatencyChart();
    }
}

themeSelect.addEventListener('change', (e) => {
    applyTheme(e.target.value);
});

// Restore user selection on startup
const savedThemeKey = safeStorage.getItem('sysThemeKey');
if (savedThemeKey && themeClasses.hasOwnProperty(savedThemeKey)) {
    applyTheme(savedThemeKey);
} else {
    // Check legacy preference
    const savedThemeIndex = safeStorage.getItem('sysThemeIndex');
    if (savedThemeIndex === '1') {
        applyTheme('bladerunner');
    } else if (savedThemeIndex === '2') {
        applyTheme('synth84');
    } else if (safeStorage.getItem('retroTheme') === 'true') {
        applyTheme('bladerunner');
    } else {
        applyTheme('modern');
    }
}

// Metrics tracking
let totalCount = 0;
let breachedCount = 0;
const sourceCounts = {};

// Ingestion Diagnostics Stats
let eventTimestamps = [];
let totalBytes = 0;
let cumulativeBytes = 0;
let sseStartTime = Date.now();
let ragHits = 0;
let fallbackHits = 0;

// Rolling latency history (last 20 events)
const latencyHistory = [];

// Fetch initial proposals list
async function fetchInitialProposals() {
    try {
        const res = await fetch('/api/proposals');
        if (res.ok) {
            const data = await res.json();
            if (data && data.length > 0) {
                if (feedEmpty) feedEmpty.remove();
                // Render reverse chronologically
                for (let i = data.length - 1; i >= 0; i--) {
                    addProposalCard(data[i], false);
                }
                updateMetrics();
            }
        }
    } catch (err) {
        console.error("Failed to load initial proposals: ", err);
    }
}

// Establish Server-Sent Events stream connection
function setupSSEStream() {
    const stream = new EventSource('/api/stream');

    stream.onopen = () => {
        statusTag.innerText = "STREAM LIVE";
        statusTag.style.color = "var(--accent-green)";
        statusTag.style.background = "rgba(16, 185, 129, 0.1)";
        statusTag.style.borderColor = "rgba(16, 185, 129, 0.2)";
        sseStartTime = Date.now();
    };

    stream.onerror = (err) => {
        statusTag.innerText = "RECONNECTING";
        statusTag.style.color = "#f59e0b";
        statusTag.style.background = "rgba(245, 158, 11, 0.1)";
        statusTag.style.borderColor = "rgba(245, 158, 11, 0.2)";
    };

    stream.addEventListener('open', (e) => {
        console.log("SSE link opened: ", e.data);
    });

    stream.onmessage = (event) => {
        try {
            const rawStr = event.data;
            const sizeBytes = new Blob([rawStr]).size;
            totalBytes += sizeBytes;
            cumulativeBytes += sizeBytes;
            eventTimestamps.push(Date.now());

            const proposal = JSON.parse(rawStr);
            if (feedEmpty) feedEmpty.remove();
            addProposalCard(proposal, true);
            updateMetrics();
        } catch (err) {
            console.error("Failed to parse event proposal payload: ", err);
        }
    };
}

function addProposalCard(p, animate = true) {
    // Stats increments
    totalCount++;
    if (p.limit_breached) breachedCount++;
    
    const source = p.source_id || "GENERIC";
    sourceCounts[source] = (sourceCounts[source] || 0) + 1;

    // RAG Hits vs Fallbacks
    if (p.model_used && p.model_used !== "rules-fallback") {
        ragHits++;
        document.getElementById('active-llm-model').innerText = p.model_used;
    } else {
        fallbackHits++;
    }

    // Update detailed developer latency history
    if (p.embed_latency_ms || p.qdrant_latency_ms || p.llm_latency_ms) {
        const emb = p.embed_latency_ms || 0;
        const qdr = p.qdrant_latency_ms || 0;
        const llm = p.llm_latency_ms || 0;
        const tot = emb + qdr + llm;

        document.getElementById('latency-embed').innerText = `${emb} ms`;
        document.getElementById('latency-qdrant').innerText = `${qdr} ms`;
        document.getElementById('latency-llm').innerText = `${llm} ms`;
        document.getElementById('latency-total').innerText = `${tot} ms`;

        // Record to history and redraw chart
        latencyHistory.push({ embed: emb, qdrant: qdr, llm: llm });
        if (latencyHistory.length > 20) {
            latencyHistory.shift();
        }
        drawLatencyChart();
    }

    const card = document.createElement('div');
    card.className = `trade-card ${animate ? 'fade-in' : ''}`;
    card.setAttribute('data-hash', p.event_hash ? p.event_hash.substring(0, 8).toUpperCase() : 'N/A');

    const actionClass = p.action.toLowerCase() === 'buy' ? 'buy' : 'sell';
    const statusClass = p.limit_breached ? 'breached' : 'ok';
    const statusText = p.limit_breached ? 'Limit Breached' : 'Risk OK';
    
    // Format Timestamp
    const ts = new Date(p.timestamp);
    const timeStr = ts.toLocaleTimeString([], { hour12: false }) + '.' + String(ts.getMilliseconds()).padStart(3, '0');

    // Format Source
    let sourceColorClass = 'source-generic';
    const srcLower = source.toLowerCase();
    if (srcLower.includes('reuters')) sourceColorClass = 'source-reuters';
    else if (srcLower.includes('twitter')) sourceColorClass = 'source-twitter';
    else if (srcLower.includes('kalshi')) sourceColorClass = 'source-kalshi';
    else if (srcLower.includes('polymarket')) sourceColorClass = 'source-polymarket';

    const sourceHTML = p.source_url 
        ? `<a href="${p.source_url}" target="_blank" class="source-tag"><span class="source-dot ${sourceColorClass}"></span>${source}</a>`
        : `<span class="source-tag"><span class="source-dot ${sourceColorClass}"></span>${source}</span>`;

    // Prepare Iceberg scheduling dots if orders exist
    let icebergHTML = '';
    if (p.iceberg_orders && p.iceberg_orders.length > 0) {
        const maxDelay = Math.max(...p.iceberg_orders.map(o => o.delay_ms)) || 1;
        const dotElements = p.iceberg_orders.map(o => {
            const pct = (o.delay_ms / maxDelay) * 95; // bound within 95% width
            return `<span class="timeline-dot" style="left: calc(${pct}% + 4px)" title="Chunk Size: ${o.size} / Delay: +${o.delay_ms}ms"></span>`;
        }).join('');

        icebergHTML = `
            <div class="iceberg-container">
                <div class="iceberg-header">
                    <span>Iceberg Schedule (${p.iceberg_orders.length} Chunks)</span>
                    <span>Total execution: ${maxDelay} ms</span>
                </div>
                <div class="timeline-bar">
                    ${dotElements}
                </div>
            </div>
        `;
    }

    // Prepare slice boundaries if slippage exceeds 1.5%
    let sliceBoundariesHTML = '';
    if (p.slice_boundaries && p.slice_boundaries.length > 0) {
        const badges = p.slice_boundaries.map(b => `<span class="slice-badge">${b}</span>`).join(' &rarr; ');
        sliceBoundariesHTML = `
            <div class="slice-boundaries">
                <span>Slice Boundaries:</span>
                ${badges}
            </div>
        `;
    }

    // Render Rationale Block
    const rationaleText = p.proposal_text || `Rule-based fallback strategy proposal for ${p.contract_id}.`;

    card.innerHTML = `
        <div class="card-top">
            <div class="badge-group">
                <span class="action-badge ${actionClass}">${p.action}</span>
                <span class="contract-badge">${p.contract_id}</span>
                ${sourceHTML}
            </div>
            <span class="status-badge ${statusClass}">${statusText}</span>
        </div>

        <div class="card-grid">
            <div class="grid-item">
                <span class="grid-label">Total Size</span>
                <span class="grid-val">${p.total_size.toLocaleString()} units</span>
            </div>
            <div class="grid-item">
                <span class="grid-label">Avg Fill Price</span>
                <span class="grid-val">$${p.average_price.toFixed(4)}</span>
            </div>
            <div class="grid-item">
                <span class="grid-label">Slippage</span>
                <span class="grid-val" style="color: ${p.slippage_pct > 0.03 ? 'var(--accent-red)' : p.slippage_pct > 0.02 ? '#f59e0b' : 'var(--accent-green)'}">
                    ${(p.slippage_pct * 100).toFixed(4)}%
                </span>
            </div>
            <div class="grid-item">
                <span class="grid-label">Latency</span>
                <span class="grid-val" style="color: var(--primary)">${p.processing_time || 'N/A'}</span>
            </div>
        </div>

        <div class="rationale-box">
            <strong>Trading Rationale:</strong><br/>
            ${rationaleText}
            ${p.limit_breached && p.breach_reason ? `<br/><span style="color: var(--accent-red); font-size: 0.85rem; font-weight:600;">⚠️ ${p.breach_reason}</span>` : ''}
        </div>

        ${sliceBoundariesHTML}
        ${icebergHTML}
    `;

    // Insert at top of feed list
    proposalsFeed.insertBefore(card, proposalsFeed.firstChild);

    // Limit list length to last 50
    if (proposalsFeed.children.length > 50) {
        proposalsFeed.removeChild(proposalsFeed.lastChild);
    }
}

function updateMetrics() {
    document.getElementById('stat-total').innerText = totalCount;
    document.getElementById('stat-breached').innerText = breachedCount;
    
    const okRate = totalCount > 0 ? (((totalCount - breachedCount) / totalCount) * 100).toFixed(1) + '%' : '100%';
    document.getElementById('stat-ok-rate').innerText = okRate;

    // RAG vs Fallback ratio
    const totalRAGRatio = totalCount > 0 ? ((ragHits / totalCount) * 100).toFixed(0) + '%' : '0%';
    document.getElementById('rag-success-ratio').innerText = `${ragHits}/${totalCount} (${totalRAGRatio})`;

    // Update source distribution sidebar list
    const sourcesContainer = document.getElementById('sources-container');
    sourcesContainer.innerHTML = '';

    Object.entries(sourceCounts).sort((a,b) => b[1] - a[1]).forEach(([source, count]) => {
        const row = document.createElement('div');
        row.className = 'metric-row';
        
        let sourceColorClass = 'source-generic';
        const srcLower = source.toLowerCase();
        if (srcLower.includes('reuters')) sourceColorClass = 'source-reuters';
        else if (srcLower.includes('twitter')) sourceColorClass = 'source-twitter';
        else if (srcLower.includes('kalshi')) sourceColorClass = 'source-kalshi';
        else if (srcLower.includes('polymarket')) sourceColorClass = 'source-polymarket';

        row.innerHTML = `
            <span class="metric-label" style="display: flex; align-items: center; gap: 0.5rem">
                <span class="source-dot ${sourceColorClass}"></span>
                ${source}
            </span>
            <span class="metric-value">${count}</span>
        `;
        sourcesContainer.appendChild(row);
    });
}

// Live Ingestion Performance Metrics Calculation Interval
setInterval(() => {
    const now = Date.now();
    
    // Filter timestamps inside the last 10 seconds
    eventTimestamps = eventTimestamps.filter(ts => now - ts < 10000);
    
    const eps = eventTimestamps.length / 10;
    document.getElementById('stat-eps').innerText = `${eps.toFixed(1)} EPS`;

    // Average Event Size
    const avgSize = totalCount > 0 ? (cumulativeBytes / totalCount) : 0;
    let avgSizeStr = `${avgSize.toFixed(0)} B`;
    if (avgSize > 1024) avgSizeStr = `${(avgSize / 1024).toFixed(1)} KB`;
    document.getElementById('stat-avg-size').innerText = avgSizeStr;

    // Ingest bandwidth (KB/s) over last 10s
    const bandwidth = (totalBytes / 1024) / 10; // 10 seconds span
    document.getElementById('stat-bandwidth').innerText = `${bandwidth.toFixed(2)} KB/s`;

    // Reset periodic counters
    totalBytes = 0;
}, 1000);

// Draw the dynamic SVG line chart
function drawLatencyChart() {
    const svg = document.getElementById('latency-svg');
    if (!svg) return;
    
    svg.innerHTML = '';
    const w = svg.clientWidth || 300;
    const h = svg.clientHeight || 160;
    
    if (latencyHistory.length === 0) {
        const txt = document.createElementNS("http://www.w3.org/2000/svg", "text");
        txt.setAttribute("x", w / 2);
        txt.setAttribute("y", h / 2);
        txt.setAttribute("fill", "var(--text-muted)");
        txt.setAttribute("font-size", "0.8rem");
        txt.setAttribute("text-anchor", "middle");
        txt.textContent = "Awaiting pipeline latency data...";
        svg.appendChild(txt);
        return;
    }
    
    // Find max value to scale Y axis
    let maxVal = 100;
    latencyHistory.forEach(d => {
        const localMax = Math.max(d.embed, d.qdrant, d.llm);
        if (localMax > maxVal) maxVal = localMax;
    });
    maxVal = Math.ceil(maxVal * 1.15); // Add 15% ceiling headroom
    
    // Draw dotted grid lines
    const gridCount = 3;
    for (let i = 0; i <= gridCount; i++) {
        const y = (i / gridCount) * h;
        const gridVal = Math.round(maxVal - (i / gridCount) * maxVal);
        
        const line = document.createElementNS("http://www.w3.org/2000/svg", "line");
        line.setAttribute("x1", 0);
        line.setAttribute("y1", y);
        line.setAttribute("x2", w);
        line.setAttribute("y2", y);
        line.setAttribute("stroke", "rgba(255, 255, 255, 0.05)");
        line.setAttribute("stroke-dasharray", "3,3");
        svg.appendChild(line);
        
        const text = document.createElementNS("http://www.w3.org/2000/svg", "text");
        text.setAttribute("x", 4);
        text.setAttribute("y", y - 4);
        text.setAttribute("fill", "var(--text-muted)");
        text.setAttribute("font-size", "0.65rem");
        text.setAttribute("opacity", "0.6");
        text.textContent = `${gridVal} ms`;
        svg.appendChild(text);
    }
    
    // Determine series colors based on active theme
    const isRoyal = document.body.classList.contains('theme-royal');
    const isBR = document.body.classList.contains('theme-bladerunner');
    const isSynth = document.body.classList.contains('theme-synth84');
    
    let embedColor = '#fb7185';
    let qdrantColor = '#60a5fa';
    let llmColor = '#a78bfa';
    
    if (isRoyal) {
        embedColor = '#d4af37'; // gold
        qdrantColor = '#2ecc71'; // emerald
        llmColor = '#3498db'; // sapphire
    } else if (isBR) {
        embedColor = '#ffb000'; // amber
        qdrantColor = '#b8860b'; // dark gold
        llmColor = '#ffd700'; // bright amber
    } else if (isSynth) {
        embedColor = '#ff007f'; // pink
        qdrantColor = '#00ffff'; // cyan
        llmColor = '#9d4edd'; // purple
    }
    
    // Sync legend colors
    document.getElementById('legend-dot-embed').style.backgroundColor = embedColor;
    document.getElementById('legend-dot-qdrant').style.backgroundColor = qdrantColor;
    document.getElementById('legend-dot-llm').style.backgroundColor = llmColor;
    
    const n = latencyHistory.length;
    const getX = (idx) => n > 1 ? (idx / (n - 1)) * (w - 20) + 10 : w / 2;
    const getY = (val) => h - (val / maxVal) * (h - 20) - 10;
    
    const drawPath = (valSelector, color) => {
        let d = '';
        latencyHistory.forEach((pt, idx) => {
            const x = getX(idx);
            const y = getY(valSelector(pt));
            if (idx === 0) d += `M ${x} ${y}`;
            else d += ` L ${x} ${y}`;
        });
        
        const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
        path.setAttribute("d", d);
        path.setAttribute("fill", "none");
        path.setAttribute("stroke", color);
        path.setAttribute("stroke-width", "2");
        path.setAttribute("stroke-linecap", "round");
        path.setAttribute("stroke-linejoin", "round");
        path.setAttribute("style", `filter: drop-shadow(0 0 2px ${color});`);
        svg.appendChild(path);
    };
    
    // Draw series paths
    drawPath(d => d.embed, embedColor);
    drawPath(d => d.qdrant, qdrantColor);
    drawPath(d => d.llm, llmColor);
    
    // Draw endpoint emphasize dots
    if (n > 0) {
        const lastIdx = n - 1;
        const last = latencyHistory[lastIdx];
        const drawDot = (val, color) => {
            const dot = document.createElementNS("http://www.w3.org/2000/svg", "circle");
            dot.setAttribute("cx", getX(lastIdx));
            dot.setAttribute("cy", getY(val));
            dot.setAttribute("r", "4");
            dot.setAttribute("fill", color);
            dot.setAttribute("stroke", "#ffffff");
            dot.setAttribute("stroke-width", "1");
            dot.setAttribute("style", `filter: drop-shadow(0 0 4px ${color});`);
            svg.appendChild(dot);
        };
        drawDot(last.embed, embedColor);
        drawDot(last.qdrant, qdrantColor);
        drawDot(last.llm, llmColor);
    }
}

window.addEventListener('resize', drawLatencyChart);

// Initialize directly on execution
fetchInitialProposals();
setupSSEStream();
