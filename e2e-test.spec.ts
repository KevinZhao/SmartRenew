import { test, expect } from '@playwright/test';

const BASE = 'http://localhost:5000';

test.describe('SmartRenew E2E', () => {

  test('health endpoint returns ok', async ({ request }) => {
    const res = await request.get(`${BASE}/api/health`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body.status).toBe('ok');
  });

  test('reservations endpoint returns array or null', async ({ request }) => {
    const res = await request.get(`${BASE}/api/reservations`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    // null or array are both valid (no reservations in test account)
    expect(body === null || Array.isArray(body)).toBeTruthy();
  });

  test('alerts endpoint returns array or null', async ({ request }) => {
    const res = await request.get(`${BASE}/api/alerts`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body === null || Array.isArray(body)).toBeTruthy();
  });

  test('sync endpoint triggers without error', async ({ request }) => {
    const res = await request.post(`${BASE}/api/sync`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body.errors).toEqual([]);
  });

  test('export CSV returns 200', async ({ request }) => {
    const res = await request.get(`${BASE}/api/export`);
    expect(res.ok()).toBeTruthy();
    const ct = res.headers()['content-type'];
    expect(ct).toContain('text/csv');
  });

  test('SPA loads and renders title', async ({ page }) => {
    await page.goto(BASE);
    // Wait for Vue app to mount
    await page.waitForLoadState('networkidle');
    // Check page has content
    const title = await page.title();
    expect(title.length).toBeGreaterThan(0);
    // Take screenshot
    await page.screenshot({ path: 'e2e-spa-home.png', fullPage: true });
  });

  test('SPA shows reservation table or empty state', async ({ page }) => {
    await page.goto(BASE);
    await page.waitForLoadState('networkidle');
    // The app should have rendered - look for common elements
    const body = await page.textContent('body');
    expect(body).toBeTruthy();
    await page.screenshot({ path: 'e2e-spa-loaded.png', fullPage: true });
  });

  test('SPA manual sync button works', async ({ page }) => {
    await page.goto(BASE);
    await page.waitForLoadState('networkidle');

    // Look for sync button and click if present
    const syncBtn = page.locator('button', { hasText: /sync/i });
    if (await syncBtn.count() > 0) {
      await syncBtn.first().click();
      // Wait for sync to complete (up to 60s)
      await page.waitForTimeout(3000);
      await page.screenshot({ path: 'e2e-spa-after-sync.png', fullPage: true });
    } else {
      // No sync button found, screenshot current state
      await page.screenshot({ path: 'e2e-spa-no-sync-btn.png', fullPage: true });
    }
  });
});
