/* Yupcha Lead Dashboard — Client-side logic */

let currentPage = 0;
const PAGE_SIZE = 50;
let searchTimer = null;

// ── Init ────────────────────────────────────────────────────
document.addEventListener("DOMContentLoaded", () => {
    loadFilters();
    loadStats();
    loadLeads();
});

function refreshData() {
    loadStats();
    loadLeads();
}

// ── Stats ───────────────────────────────────────────────────
async function loadStats() {
    try {
        const res = await fetch("/api/stats");
        const s = await res.json();
        document.getElementById("statTotal").textContent = s.total;
        document.getElementById("statHot").textContent = s.by_tier?.hot || 0;
        document.getElementById("statWarm").textContent = s.by_tier?.warm || 0;
        document.getElementById("statCold").textContent = s.by_tier?.cold || 0;
        document.getElementById("statAvgScore").textContent = Math.round(s.enrichment?.avg_score || 0);
        document.getElementById("statEnriched").textContent = s.enrichment?.with_email || 0;
        renderCityChart(s.by_city || {});
        renderSourceChart(s.by_source || {});
        renderTierChart(s.by_tier || {});
    } catch (e) {
        console.error("Stats error:", e);
    }
}

// ── Charts ──────────────────────────────────────────────────
function renderCityChart(data) {
    const el = document.getElementById("cityChart");
    const entries = Object.entries(data).slice(0, 8);
    const max = Math.max(...entries.map(e => e[1]), 1);
    const colors = ["#6366f1", "#8b5cf6", "#a78bfa", "#c4b5fd", "#6366f1", "#818cf8", "#a5b4fc", "#c7d2fe"];
    el.innerHTML = entries.map(([city, count], i) => `
        <div class="bar-row">
            <span class="bar-label">${city}</span>
            <div class="bar-track">
                <div class="bar-fill" style="width:${(count/max)*100}%;background:${colors[i%colors.length]}">${count}</div>
            </div>
        </div>
    `).join("");
}

function renderSourceChart(data) {
    const el = document.getElementById("sourceChart");
    const entries = Object.entries(data).slice(0, 8);
    const max = Math.max(...entries.map(e => e[1]), 1);
    const colors = ["#10b981", "#06b6d4", "#3b82f6", "#8b5cf6", "#f59e0b", "#ef4444", "#ec4899", "#14b8a6"];
    el.innerHTML = entries.map(([src, count], i) => `
        <div class="bar-row">
            <span class="bar-label">${src}</span>
            <div class="bar-track">
                <div class="bar-fill" style="width:${(count/max)*100}%;background:${colors[i%colors.length]}">${count}</div>
            </div>
        </div>
    `).join("");
}

function renderTierChart(data) {
    const el = document.getElementById("tierChart");
    const tiers = [
        { key: "hot", label: "Hot", color: "#ef4444" },
        { key: "warm", label: "Warm", color: "#f59e0b" },
        { key: "cold", label: "Cold", color: "#3b82f6" },
        { key: "unqualified", label: "Unqualified", color: "#52525b" },
    ];
    const total = tiers.reduce((s, t) => s + (data[t.key] || 0), 0) || 1;

    // SVG donut
    let cumulativePercent = 0;
    const size = 140, stroke = 20, radius = (size - stroke) / 2;
    const circumference = 2 * Math.PI * radius;
    let segments = "";

    tiers.forEach(t => {
        const pct = (data[t.key] || 0) / total;
        const offset = circumference * cumulativePercent;
        const dash = circumference * pct;
        segments += `<circle cx="${size/2}" cy="${size/2}" r="${radius}" fill="none" stroke="${t.color}"
            stroke-width="${stroke}" stroke-dasharray="${dash} ${circumference - dash}"
            stroke-dashoffset="-${offset}" style="transition: all 0.6s ease"/>`;
        cumulativePercent += pct;
    });

    const legend = tiers.map(t => `
        <div class="tier-item">
            <div class="tier-dot" style="background:${t.color}"></div>
            <span>${t.label}: ${data[t.key] || 0}</span>
        </div>
    `).join("");

    el.innerHTML = `
        <svg width="${size}" height="${size}" viewBox="0 0 ${size} ${size}">
            <circle cx="${size/2}" cy="${size/2}" r="${radius}" fill="none" stroke="#1e1e2e" stroke-width="${stroke}"/>
            ${segments}
            <text x="${size/2}" y="${size/2}" text-anchor="middle" dominant-baseline="central"
                  fill="#e4e4e7" font-size="22" font-weight="800">${total}</text>
        </svg>
        <div class="tier-legend">${legend}</div>
    `;
}

// ── Filters ─────────────────────────────────────────────────
async function loadFilters() {
    try {
        const res = await fetch("/api/filters");
        const f = await res.json();
        const citySelect = document.getElementById("filterCity");
        f.cities.forEach(c => {
            citySelect.innerHTML += `<option value="${c}">${c}</option>`;
        });
        const sourceSelect = document.getElementById("filterSource");
        f.sources.forEach(s => {
            sourceSelect.innerHTML += `<option value="${s}">${s}</option>`;
        });
    } catch (e) {
        console.error("Filters error:", e);
    }
}

function debounceSearch() {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => { currentPage = 0; loadLeads(); }, 300);
}

// ── Leads Table ─────────────────────────────────────────────
async function loadLeads() {
    const params = new URLSearchParams();
    const search = document.getElementById("searchInput").value;
    const city = document.getElementById("filterCity").value;
    const tier = document.getElementById("filterTier").value;
    const status = document.getElementById("filterStatus").value;
    const source = document.getElementById("filterSource").value;
    const orderBy = document.getElementById("sortBy").value;

    if (search) params.set("search", search);
    if (city) params.set("city", city);
    if (tier) params.set("tier", tier);
    if (status) params.set("status", status);
    if (source) params.set("source", source);
    params.set("order_by", orderBy);
    params.set("limit", PAGE_SIZE);
    params.set("offset", currentPage * PAGE_SIZE);

    try {
        const res = await fetch(`/api/leads?${params}`);
        const leads = await res.json();
        renderLeads(leads);
        document.getElementById("resultCount").textContent = `${leads.length} leads shown`;
        document.getElementById("pageInfo").textContent = `Page ${currentPage + 1}`;
    } catch (e) {
        console.error("Leads error:", e);
    }
}

function renderLeads(leads) {
    const tbody = document.getElementById("leadsBody");
    if (!leads.length) {
        tbody.innerHTML = `<tr><td colspan="9" style="text-align:center;padding:40px;color:var(--text-muted)">No leads found</td></tr>`;
        return;
    }
    tbody.innerHTML = leads.map(l => `
        <tr onclick="showDetail(${l.id})">
            <td><span class="score-badge ${l.score_tier}">${l.score}</span></td>
            <td><strong>${esc(l.company)}</strong></td>
            <td>${esc(l.city)}</td>
            <td>${l.email ? `<a href="mailto:${esc(l.email)}" onclick="event.stopPropagation()">${esc(l.email)}</a>` : '<span style="color:var(--text-dim)">—</span>'}</td>
            <td>${l.phone ? `<a href="tel:${esc(l.phone)}" onclick="event.stopPropagation()">${esc(l.phone)}</a>` : '<span style="color:var(--text-dim)">—</span>'}</td>
            <td>${esc(l.specialization)}</td>
            <td><span class="source-tag">${esc(l.source)}</span></td>
            <td><span class="status-badge status-${l.status}">${l.status}</span></td>
            <td>
                <button class="action-btn" onclick="event.stopPropagation(); changeStatus(${l.id})" title="Change status">⚡</button>
                <button class="action-btn danger" onclick="event.stopPropagation(); deleteLead(${l.id})" title="Delete">✕</button>
            </td>
        </tr>
    `).join("");
}

function esc(str) {
    if (!str) return "";
    const el = document.createElement("span");
    el.textContent = str;
    return el.innerHTML;
}

function prevPage() { if (currentPage > 0) { currentPage--; loadLeads(); } }
function nextPage() { currentPage++; loadLeads(); }

// ── Lead Detail ─────────────────────────────────────────────
async function showDetail(id) {
    try {
        const res = await fetch(`/api/lead/${id}`);
        const l = await res.json();
        document.getElementById("modalTitle").textContent = l.company;
        document.getElementById("modalBody").innerHTML = `
            <div class="detail-grid">
                <div class="detail-item"><label>Score</label><div class="value"><span class="score-badge ${l.score_tier}">${l.score}</span> ${l.score_tier}</div></div>
                <div class="detail-item"><label>Status</label><div class="value"><span class="status-badge status-${l.status}">${l.status}</span></div></div>
                <div class="detail-item"><label>City</label><div class="value">${l.city || "—"}</div></div>
                <div class="detail-item"><label>Specialization</label><div class="value">${l.specialization || "—"}</div></div>
                <div class="detail-item"><label>Website</label><div class="value">${l.website ? `<a href="${l.website}" target="_blank">${l.website}</a>` : "—"}</div></div>
                <div class="detail-item"><label>Email</label><div class="value">${l.email ? `<a href="mailto:${l.email}">${l.email}</a>` : "—"}</div></div>
                <div class="detail-item"><label>Phone</label><div class="value">${l.phone ? `<a href="tel:${l.phone}">${l.phone}</a>` : "—"}</div></div>
                <div class="detail-item"><label>LinkedIn</label><div class="value">${l.linkedin_url ? `<a href="${l.linkedin_url}" target="_blank">View Profile</a>` : "—"}</div></div>
                <div class="detail-item"><label>Contact Person</label><div class="value">${l.contact_person || "—"}</div></div>
                <div class="detail-item"><label>Contact Title</label><div class="value">${l.contact_title || "—"}</div></div>
                <div class="detail-item"><label>Company Size</label><div class="value">${l.company_size || "—"}</div></div>
                <div class="detail-item"><label>Source</label><div class="value"><span class="source-tag">${l.source}</span></div></div>
                <div class="detail-item full"><label>Value Proposition</label><div class="value">${l.yupcha_value_prop || "—"}</div></div>
                <div class="detail-item full"><label>Notes</label><div class="value">${l.notes || "—"}</div></div>
            </div>
        `;
        document.getElementById("detailModal").classList.add("active");
    } catch (e) {
        console.error("Detail error:", e);
    }
}

function hideModal() { document.getElementById("detailModal").classList.remove("active"); }
function closeModal(e) { if (e.target.classList.contains("modal-overlay")) e.target.classList.remove("active"); }

// ── Status Change ───────────────────────────────────────────
async function changeStatus(id) {
    const statuses = ["new", "contacted", "qualified", "negotiating", "converted", "dead"];
    const status = prompt(`Set status for lead #${id}:\n${statuses.join(", ")}`);
    if (!status || !statuses.includes(status)) return;
    try {
        await fetch(`/api/lead/${id}/status`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ status }),
        });
        loadLeads();
        loadStats();
    } catch (e) {
        alert("Error updating status");
    }
}

// ── Delete ──────────────────────────────────────────────────
async function deleteLead(id) {
    if (!confirm("Delete this lead?")) return;
    try {
        await fetch(`/api/lead/${id}`, { method: "DELETE" });
        loadLeads();
        loadStats();
    } catch (e) {
        alert("Error deleting lead");
    }
}

// ── Add Lead ────────────────────────────────────────────────
function showAddModal() { document.getElementById("addModal").classList.add("active"); }
function hideAddModal() { document.getElementById("addModal").classList.remove("active"); }

async function submitNewLead(e) {
    e.preventDefault();
    const form = document.getElementById("addForm");
    const data = Object.fromEntries(new FormData(form));
    try {
        await fetch("/api/lead", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(data),
        });
        form.reset();
        hideAddModal();
        loadLeads();
        loadStats();
    } catch (e) {
        alert("Error adding lead");
    }
}

// ── Export ───────────────────────────────────────────────────
function exportCSV() {
    const params = new URLSearchParams();
    const tier = document.getElementById("filterTier").value;
    const status = document.getElementById("filterStatus").value;
    const city = document.getElementById("filterCity").value;
    if (tier) params.set("tier", tier);
    if (status) params.set("status", status);
    if (city) params.set("city", city);
    window.location.href = `/api/export/csv?${params}`;
}
