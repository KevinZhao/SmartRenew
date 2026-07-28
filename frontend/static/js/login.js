const { createApp, ref } = Vue;

createApp({
    setup() {
        const username = ref('');
        const password = ref('');
        const error = ref('');
        const busy = ref(false);

        async function submit() {
            if (busy.value) return;
            error.value = '';
            busy.value = true;
            try {
                const res = await fetch('/api/login', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ username: username.value, password: password.value }),
                });
                if (!res.ok) {
                    const body = await res.json().catch(() => ({}));
                    throw new Error(body.error || `HTTP ${res.status}`);
                }
                // Only same-origin paths are honoured, so ?next= cannot be used
                // as an open redirect.
                const next = new URLSearchParams(window.location.search).get('next');
                window.location.replace(next && next.startsWith('/') && !next.startsWith('//') ? next : '/');
            } catch (e) {
                error.value = e.message;
                password.value = '';
                busy.value = false;
            }
        }

        return { username, password, error, busy, submit };
    },
}).mount('#login-app');
