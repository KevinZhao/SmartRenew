const { createApp, ref, computed, onMounted } = Vue;

createApp({
    setup() {
        const activeTab = ref('dashboard');
        const reservations = ref([]);
        const alerts = ref([]);
        const accounts = ref([]);
        const loading = ref(false);
        const accountsLoading = ref(false);
        const filterType = ref('');
        const filterAccount = ref('');
        const searchText = ref('');
        const accountSearch = ref('');
        const message = ref('');
        const messageErr = ref(false);
        const editingAccountId = ref(null);
        const editingTag = ref('');

        function showMsg(text, isErr = false) {
            message.value = text;
            messageErr.value = isErr;
            setTimeout(() => { message.value = ''; }, 5000);
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

        async function loadAccounts() {
            accountsLoading.value = true;
            try {
                accounts.value = await apiFetch('/api/accounts');
            } catch (e) {
                showMsg('Load accounts failed: ' + e.message, true);
                accounts.value = [];
            } finally {
                accountsLoading.value = false;
            }
        }

        function startEditTag(account) {
            editingAccountId.value = account.account_id;
            editingTag.value = account.tag || '';
        }

        function cancelEditTag() {
            editingAccountId.value = null;
            editingTag.value = '';
        }

        async function saveTag(account) {
            try {
                await apiFetch('/api/accounts/tag', {
                    method: 'PUT',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ account_id: account.account_id, tag: editingTag.value })
                });
                account.tag = editingTag.value;
                editingAccountId.value = null;
                editingTag.value = '';
                showMsg('Tag updated');
            } catch (e) {
                showMsg('Update tag failed: ' + e.message, true);
            }
        }

        async function syncData() {
            showMsg('Syncing from AWS...');
            try {
                const data = await apiFetch('/api/sync', { method: 'POST' });
                const errCount = data.errors ? data.errors.length : 0;
                showMsg(`Synced ${data.synced} items` + (errCount ? `, ${errCount} errors` : ''));
                loadReservations();
                loadAlerts();
            } catch (e) {
                showMsg('Sync failed: ' + e.message, true);
            }
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
            return (reservations.value || []).filter(r => {
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
        });

        const stats = computed(() => {
            const now = new Date();
            const list = reservations.value || [];
            let total = list.length, expired = 0, critical = 0, warning = 0, active = 0;
            let sp = 0, cb = 0, odcr = 0, ri = 0;
            list.forEach(r => {
                if (r.type === 'sp') sp++;
                if (r.type === 'cb') cb++;
                if (r.type === 'odcr') odcr++;
                if (r.type === 'ri') ri++;
                if (r.end_time) {
                    const days = (new Date(r.end_time) - now) / 86400000;
                    if (days <= 0) expired++;
                    else if (days <= 7) critical++;
                    else if (days <= 30) warning++;
                    else active++;
                } else { active++; }
            });
            return { total, expired, critical, warning, active, sp, cb, odcr, ri };
        });

        const uniqueAccounts = computed(() => {
            const seen = new Set();
            return (reservations.value || []).filter(r => {
                if (seen.has(r.account_id)) return false;
                seen.add(r.account_id);
                return true;
            });
        });

        const filteredAccounts = computed(() => {
            return (accounts.value || []).filter(a => {
                if (!accountSearch.value) return true;
                const s = accountSearch.value.toLowerCase();
                return (a.account_name || '').toLowerCase().includes(s)
                    || (a.account_id || '').toLowerCase().includes(s)
                    || (a.email || '').toLowerCase().includes(s)
                    || (a.tag || '').toLowerCase().includes(s);
            });
        });

        function formatDate(d) {
            if (!d) return '-';
            try { return new Date(d).toLocaleDateString('zh-CN'); } catch { return d; }
        }

        function daysUntil(d) {
            if (!d) return null;
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

        onMounted(() => {
            loadReservations();
        });

        return {
            activeTab, reservations, alerts, accounts, loading, accountsLoading,
            filterType, filterAccount, searchText, accountSearch,
            message, messageErr,
            editingAccountId, editingTag,
            filteredReservations, filteredAccounts, stats, uniqueAccounts,
            syncData, exportCSV, importFile,
            loadAlerts, loadAccounts, startEditTag, cancelEditTag, saveTag,
            formatDate, daysUntil, daysClass, daysDisplay,
            levelColor, levelText,
        };
    }
}).mount('#app');
