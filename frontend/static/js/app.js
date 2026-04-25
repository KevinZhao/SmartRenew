const { createApp, ref, computed, onMounted } = Vue;

createApp({
    setup() {
        const activeTab = ref('dashboard');
        const reservations = ref([]);
        const alerts = ref([]);
        const loading = ref(false);
        const filterType = ref('');
        const filterAccount = ref('');
        const searchText = ref('');
        const message = ref('');
        const messageErr = ref(false);
        const gpuCoverage = ref([]);
        const gpuFilterAccount = ref('');
        const gpuSearchText = ref('');
        const sortKey = ref('');
        const sortDir = ref('asc');
        const gpuSortKey = ref('');
        const gpuSortDir = ref('asc');

        function sortBy(key) {
            if (sortKey.value === key) {
                sortDir.value = sortDir.value === 'asc' ? 'desc' : 'asc';
            } else {
                sortKey.value = key;
                sortDir.value = 'asc';
            }
        }

        function gpuSortBy(key) {
            if (gpuSortKey.value === key) {
                gpuSortDir.value = gpuSortDir.value === 'asc' ? 'desc' : 'asc';
            } else {
                gpuSortKey.value = key;
                gpuSortDir.value = 'asc';
            }
        }

        function sortArrow(current, key, dir) {
            if (current !== key) return '\u2195';
            return dir === 'asc' ? '\u2191' : '\u2193';
        }

        function compareValues(a, b, key) {
            let va = a[key], vb = b[key];
            if (key === 'start_time' || key === 'end_time') {
                va = va && !isZeroTime(va) ? new Date(va).getTime() : -Infinity;
                vb = vb && !isZeroTime(vb) ? new Date(vb).getTime() : -Infinity;
            } else if (typeof va === 'number' || typeof vb === 'number') {
                va = Number(va) || 0;
                vb = Number(vb) || 0;
            } else {
                va = (va == null ? '' : String(va)).toLowerCase();
                vb = (vb == null ? '' : String(vb)).toLowerCase();
            }
            if (va < vb) return -1;
            if (va > vb) return 1;
            return 0;
        }

        let msgTimer = null;
        function showMsg(text, isErr = false) {
            message.value = text;
            messageErr.value = isErr;
            if (msgTimer) clearTimeout(msgTimer);
            msgTimer = setTimeout(() => { message.value = ''; msgTimer = null; }, 5000);
        }

        async function apiFetch(url, opts) {
            const res = await fetch(url, opts);
            if (!res.ok) {
                const body = await res.json().catch(() => ({}));
                throw new Error(body.error || `HTTP ${res.status}`);
            }
            return res.json();
        }

        async function loadReservations() {
            loading.value = true;
            try {
                reservations.value = await apiFetch('/api/reservations');
            } catch (e) {
                showMsg('Load failed: ' + e.message, true);
                reservations.value = [];
            } finally {
                loading.value = false;
            }
        }

        async function loadAlerts() {
            try {
                alerts.value = await apiFetch('/api/alerts');
            } catch (e) {
                showMsg('Load alerts failed: ' + e.message, true);
                alerts.value = [];
            }
        }

        async function loadGPUCoverage() {
            try {
                gpuCoverage.value = await apiFetch('/api/gpu-coverage');
            } catch (e) {
                showMsg('Load GPU coverage failed: ' + e.message, true);
                gpuCoverage.value = [];
            }
        }

        const syncRunning = ref(false);
        let syncPollTimer = null;
        const SYNC_POLL_TIMEOUT_MS = 10 * 60 * 1000; // hard cap so UI never spins forever
        const SYNC_POLL_MAX_CONSECUTIVE_ERRS = 3;

        async function syncData() {
            if (syncRunning.value) {
                showMsg('Sync already running, please wait...');
                return;
            }
            showMsg('Syncing from AWS... (this may take a few minutes)');
            syncRunning.value = true;
            try {
                // Fire-and-forget — backend returns 202 (new run) or 409 (already running).
                const res = await fetch('/api/sync', { method: 'POST' });
                if (res.status !== 202 && res.status !== 409 && !res.ok) {
                    const body = await res.json().catch(() => ({}));
                    throw new Error(body.error || `HTTP ${res.status}`);
                }
                pollSyncStatus();
            } catch (e) {
                syncRunning.value = false;
                showMsg('Sync failed to start: ' + e.message, true);
            }
        }

        function pollSyncStatus() {
            if (syncPollTimer) { clearTimeout(syncPollTimer); syncPollTimer = null; }
            const pollStart = Date.now();
            let consecErrs = 0;
            const tick = async () => {
                if (Date.now() - pollStart > SYNC_POLL_TIMEOUT_MS) {
                    syncRunning.value = false;
                    syncPollTimer = null;
                    showMsg('Sync is still running on the server (UI gave up after 10 minutes). Refresh later to see results.', true);
                    return;
                }
                try {
                    const s = await apiFetch('/api/sync/status');
                    consecErrs = 0;
                    if (s.running) {
                        syncRunning.value = true;
                        syncPollTimer = setTimeout(tick, 5000);
                        return;
                    }
                    syncRunning.value = false;
                    syncPollTimer = null;
                    const errCount = s.errors ? s.errors.length : 0;
                    showMsg(`Synced ${s.synced || 0} items` + (errCount ? `, ${errCount} errors` : ''), errCount > 0);
                    loadReservations();
                    loadAlerts();
                    loadGPUCoverage();
                } catch (e) {
                    consecErrs += 1;
                    if (consecErrs >= SYNC_POLL_MAX_CONSECUTIVE_ERRS) {
                        syncRunning.value = false;
                        syncPollTimer = null;
                        showMsg('Status check failed (sync may still be running on the server): ' + e.message, true);
                        return;
                    }
                    // Transient error — keep polling.
                    syncPollTimer = setTimeout(tick, 5000);
                }
            };
            // First probe after a short delay so the goroutine has a chance to flip running=true.
            syncPollTimer = setTimeout(tick, 2000);
        }

        function exportCSV() {
            window.open('/api/export', '_blank');
        }

        async function importFile(event) {
            const file = event.target.files[0];
            if (!file) return;
            const formData = new FormData();
            formData.append('file', file);
            try {
                const data = await apiFetch('/api/import', { method: 'POST', body: formData });
                showMsg(`Imported ${data.imported || 0} records`);
                loadReservations();
                loadAlerts();
            } catch (e) {
                showMsg('Import failed: ' + e.message, true);
            }
            event.target.value = '';
        }

        const filteredReservations = computed(() => {
            const list = (reservations.value || []).filter(r => {
                if (filterType.value && r.type !== filterType.value) return false;
                if (filterAccount.value && r.account_id !== filterAccount.value) return false;
                if (searchText.value) {
                    const s = searchText.value.toLowerCase();
                    return (r.instance_type || '').toLowerCase().includes(s)
                        || (r.resource_id || '').toLowerCase().includes(s)
                        || (r.account_alias || '').toLowerCase().includes(s)
                        || (r.description || '').toLowerCase().includes(s);
                }
                return true;
            });
            if (!sortKey.value) return list;
            const key = sortKey.value;
            const sign = sortDir.value === 'asc' ? 1 : -1;
            const sorted = list.slice().sort((a, b) => {
                if (key === 'days_left') {
                    const da = daysUntil(a.end_time);
                    const db = daysUntil(b.end_time);
                    const va = da === null ? Infinity : da;
                    const vb = db === null ? Infinity : db;
                    return (va - vb) * sign;
                }
                if (key === 'usage') {
                    const ua = (a.type === 'odcr' || a.type === 'cb') ? (a.used_count || 0) : -1;
                    const ub = (b.type === 'odcr' || b.type === 'cb') ? (b.used_count || 0) : -1;
                    return (ua - ub) * sign;
                }
                return compareValues(a, b, key) * sign;
            });
            return sorted;
        });

        const stats = computed(() => {
            const now = new Date();
            const list = reservations.value || [];
            let total = list.length, expired = 0, critical = 0, warning = 0, active = 0;
            let sp = 0, cb = 0, odcr = 0, ri = 0, idle = 0;
            list.forEach(r => {
                if (r.type === 'sp') sp++;
                if (r.type === 'cb') cb++;
                if (r.type === 'odcr') odcr++;
                if (r.type === 'ri') ri++;
                if ((r.type === 'odcr' || r.type === 'cb') && r.used_count < r.quantity && r.status === 'active') idle++;
                if (r.end_time && !isZeroTime(r.end_time)) {
                    const days = (new Date(r.end_time) - now) / 86400000;
                    if (days <= 0) expired++;
                    else if (days <= 7) critical++;
                    else if (days <= 30) warning++;
                    else active++;
                } else { active++; }
            });
            return { total, expired, critical, warning, active, sp, cb, odcr, ri, idle };
        });

        const uniqueAccounts = computed(() => {
            const seen = new Set();
            return (reservations.value || []).filter(r => {
                if (seen.has(r.account_id)) return false;
                seen.add(r.account_id);
                return true;
            });
        });

        function isZeroTime(d) {
            if (!d) return true;
            return new Date(d).getFullYear() <= 1;
        }

        function formatDate(d) {
            if (isZeroTime(d)) return '-';
            try { return new Date(d).toLocaleDateString('zh-CN'); } catch { return d; }
        }

        function daysUntil(d) {
            if (isZeroTime(d)) return null;
            return Math.ceil((new Date(d) - new Date()) / 86400000);
        }

        function daysClass(days) {
            if (days === null) return '';
            if (days <= 0) return 'badge badge-critical';
            if (days <= 7) return 'badge badge-critical';
            if (days <= 30) return 'badge badge-warning';
            return 'badge badge-normal';
        }

        function daysDisplay(days) {
            if (days === null) return '-';
            if (days <= 0) return 'Expired';
            return days + 'd';
        }

        function levelColor(level) {
            const map = { critical: '#c62828', warning: '#e65100', attention: '#f57f17', normal: '#2e7d32' };
            return map[level] || '#333';
        }

        function levelText(level) {
            const map = { critical: 'Urgent', warning: 'Warning', attention: 'Attention', normal: 'OK' };
            return map[level] || level;
        }

        const gpuStats = computed(() => {
            const list = gpuCoverage.value || [];
            let total = list.length, onDemand = 0, sp = 0, cb = 0, ri = 0;
            list.forEach(g => {
                if (g.coverage === 'on_demand') onDemand++;
                else if (g.coverage === 'savings_plan') sp++;
                else if (g.coverage === 'capacity_block') cb++;
                else if (g.coverage === 'reserved_instance') ri++;
            });
            return { total, onDemand, sp, cb, ri };
        });

        const filteredGPUCoverage = computed(() => {
            const list = (gpuCoverage.value || []).filter(g => {
                if (gpuFilterAccount.value && g.account_id !== gpuFilterAccount.value) return false;
                if (gpuSearchText.value) {
                    const s = gpuSearchText.value.toLowerCase();
                    return (g.instance_id || '').toLowerCase().includes(s)
                        || (g.instance_type || '').toLowerCase().includes(s)
                        || (g.account_alias || '').toLowerCase().includes(s)
                        || (g.coverage_ref || '').toLowerCase().includes(s);
                }
                return true;
            });
            if (!gpuSortKey.value) return list;
            const key = gpuSortKey.value;
            const sign = gpuSortDir.value === 'asc' ? 1 : -1;
            return list.slice().sort((a, b) => compareValues(a, b, key) * sign);
        });

        const gpuFamilyPrefixes = ['p3','p4','p5','p6','g4','g5','g6','g7','inf','trn','dl'];
        function isGpuFamily(family) {
            if (!family) return false;
            const f = family.toLowerCase();
            return gpuFamilyPrefixes.some(p => f.startsWith(p));
        }
        function formatCapacity(r) {
            if (r.type !== 'sp' || !r.equiv_cores || r.equiv_cores <= 0) return '-';
            const n = r.equiv_cores;
            const unit = isGpuFamily(r.instance_type) ? 'cards' : 'cores';
            // Compute SP is approximate (depends on reference instance choice);
            // family-scoped SPs are exact.
            const isCompute = r.platform === 'Compute';
            const val = n >= 1 ? (isCompute ? Math.round(n) : n.toFixed(n >= 10 ? 0 : 2)) : n.toFixed(2);
            const prefix = isCompute ? '~' : '';
            return prefix + val + ' ' + unit;
        }

        function coverageLabel(coverage) {
            const map = { on_demand: 'On-Demand', savings_plan: 'Savings Plan', capacity_block: 'Capacity Block', reserved_instance: 'Reserved Instance' };
            return map[coverage] || coverage;
        }

        function coverageBadgeClass(coverage) {
            const map = { on_demand: 'badge-gpu_od', savings_plan: 'badge-gpu_sp', capacity_block: 'badge-gpu_cb', reserved_instance: 'badge-gpu_ri' };
            return map[coverage] || '';
        }

        // If a sync is still running (e.g. triggered by a previous user session),
        // pick up its status on page load so the UI stays coherent.
        async function resumeSyncIfRunning() {
            try {
                const s = await apiFetch('/api/sync/status');
                // Guard against racing with a concurrent click that already started polling.
                if (s.running && !syncPollTimer) {
                    syncRunning.value = true;
                    showMsg('Sync in progress...');
                    pollSyncStatus();
                }
            } catch (_) { /* ignore */ }
        }

        onMounted(() => {
            loadReservations();
            loadGPUCoverage();
            resumeSyncIfRunning();
        });

        return {
            activeTab, reservations, alerts, loading,
            filterType, filterAccount, searchText,
            message, messageErr,
            filteredReservations, stats, uniqueAccounts,
            gpuCoverage, gpuFilterAccount, gpuSearchText,
            gpuStats, filteredGPUCoverage,
            sortKey, sortDir, sortBy,
            gpuSortKey, gpuSortDir, gpuSortBy,
            sortArrow,
            syncData, syncRunning, exportCSV, importFile,
            loadAlerts, loadGPUCoverage,
            formatCapacity, formatDate, daysUntil, daysClass, daysDisplay,
            levelColor, levelText,
            coverageLabel, coverageBadgeClass,
        };
    }
}).mount('#app');
