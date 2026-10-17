import { expect, test, type Page } from '@playwright/test';

import {
  assertCardDAVForbiddenMarkersAbsent,
  CARD_DAV_FORBIDDEN_MARKERS,
  installCardDAV
} from './e2e/fixtures/carddav';
import { installMixedArchive } from './e2e/fixtures/mixed-archive';

// Browser boundary: Playwright proves the built Svelte application's request
// construction, state transitions, keyboard/focus behavior, responsive UI,
// and accessibility through intercepted HTTP transport. It is not live
// CardDAV-server or live Go-backend integration. Task 4 generated-client/API
// tests and the controller request tests own exact wire-contract proof.

test('fixture never records credentials from an unmatched CardDAV account route', async ({ page }) => {
  const fixture = await installCardDAV(page, { configured: true });
  await page.goto('/');

  const status = await page.evaluate(async (password) => {
    const response = await fetch('/api/v1/carddav/account/unmodeled', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        base_url: 'https://carddav.example.test/',
        username: 'synthetic-user',
        password,
        enabled: true,
        schedule: '0 2 * * *',
        unexpected: 'not recorded'
      })
    });
    return response.status;
  }, fixture.password);

  expect(status).toBe(404);
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/account/unmodeled')).toEqual([{
    method: 'POST',
    path: '/api/v1/carddav/account/unmodeled',
    query: {},
    body: {
      base_url: 'https://carddav.example.test/',
      username: 'synthetic-user',
      enabled: true,
      schedule: '0 2 * * *'
    }
  }]);
  expect(JSON.stringify(fixture.requests)).not.toContain(fixture.password);
});

test('keyboard journey configures CardDAV, reconciles roles, syncs history, and resolves a conflict', async ({ page }) => {
  const fixture = await installCardDAV(page);
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);

  const cardDAVCategory = page.getByRole('button', { name: /^CardDAV account/ });
  await cardDAVCategory.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: 'CardDAV account' })).toBeVisible();
  await expect(page.getByText('Not configured', { exact: true })).toBeVisible();
  await expect(page.getByRole('alert')).toHaveText('CardDAV account discovery is missing. Test and save the account again.');
  expect(fixture.requests.filter(({ path }) => /carddav\/(books|conflicts)$/.test(path))).toEqual([]);

  await page.getByLabel('Base URL').fill('https://carddav.example.test/');
  await page.getByLabel('Username').fill('synthetic-user');
  await page.getByLabel('Password').fill(fixture.password);
  const enabled = page.getByRole('switch', { name: 'Enabled' });
  await enabled.focus();
  await page.keyboard.press('Space');
  const presets = page.getByRole('combobox', { name: 'Presets: Off' });
  await presets.focus();
  await page.keyboard.press('Enter');
  await page.getByRole('option', { name: 'Custom' }).click();
  await page.getByLabel('Schedule').fill('0 2 * * *');

  const testConnection = page.getByRole('button', { name: 'Test CardDAV connection' });
  await testConnection.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByText('Connection successful. Found 2 address books.')).toBeVisible();

  const save = page.getByRole('button', { name: 'Save CardDAV account' });
  await save.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByText('CardDAV account saved. Found 2 address books.')).toBeVisible();

  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/account/test')).toEqual([{
    method: 'POST',
    path: '/api/v1/carddav/account/test',
    query: {},
    body: {
      connection: 'default',
      base_url: 'https://carddav.example.test/',
      username: 'synthetic-user',
      enabled: true,
      schedule: '0 2 * * *'
    },
    passwordMatched: true
  }]);
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/account')).toEqual([{
    method: 'PUT',
    path: '/api/v1/carddav/account',
    query: {},
    body: {
      connection: 'default',
      base_url: 'https://carddav.example.test/',
      username: 'synthetic-user',
      enabled: true,
      schedule: '0 2 * * *'
    },
    passwordMatched: true
  }]);
  expect(JSON.stringify(fixture.requests)).not.toContain(fixture.password);

  await expect(page.getByLabel('CardDAV status summary')).toContainText('Configured');
  await expect(page.getByLabel('CardDAV status summary')).toContainText('Runtime available');
  await expect(page.getByText('Scheduled sync enabled')).toBeVisible();

  const publishHere = page.getByRole('checkbox', { name: 'Publish here for Synthetic publishing' });
  await publishHere.focus();
  await page.keyboard.press('Space');
  await expect(publishHere).toBeChecked();
  await expect(page.getByRole('checkbox', { name: 'Sync contacts for Synthetic publishing' })).toBeChecked();

  const applyRoles = page.getByRole('button', { name: 'Apply roles for Synthetic publishing' });
  await applyRoles.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('alert')).toContainText('Current books were refreshed; review and apply again.');
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/books/6')).toEqual([{
    method: 'PATCH',
    path: '/api/v1/carddav/books/6',
    query: {},
    body: { subscribed: true, lookup_source: false, write_target: true }
  }]);

  await applyRoles.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByText('Address-book roles saved. All books were refreshed.')).toBeVisible();
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/books/6')).toHaveLength(2);
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/books/6')[1]?.body).toEqual({
    subscribed: true,
    lookup_source: false,
    write_target: true
  });

  const sync = page.getByRole('button', { name: 'Sync now' });
  await sync.focus();
  await page.keyboard.press('Enter');
  const activeSync = page.getByLabel('Active CardDAV sync');
  await expect(activeSync).toContainText('1 updated');
  await expect(activeSync).toContainText('2 updated');
  await expect(page.getByLabel('Latest CardDAV sync')).toContainText('Succeeded');
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/sync')).toEqual([{
    method: 'POST', path: '/api/v1/carddav/sync', query: {}, body: { full: false, connection: 'default' }
  }]);

  const loadMore = page.getByRole('button', { name: 'Load more history' });
  await loadMore.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('table', { name: 'CardDAV sync history' }).getByRole('row')).toHaveCount(4);
  expect(fixture.requests.filter(({ path, query }) => path === '/api/v1/carddav/runs' && query.before_id === '90'))
    .toEqual([{ method: 'GET', path: '/api/v1/carddav/runs', query: { connection: 'default', limit: '25', before_id: '90' } }]);

  const conflictRow = page.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' });
  await conflictRow.focus();
  await page.keyboard.press('Enter');

  await submitConflictChoice(page, 'Use msgvault version', 'Restore in address book');
  await expect(page.getByRole('alert')).toContainText('The latest versions are shown; choose again.');
  const refreshedChoice = page.getByRole('button', { name: 'Use msgvault version' }).first();
  await expect(refreshedChoice).toBeFocused();
  await expect(refreshedChoice).toBeEnabled();
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/conflicts/41/resolve')).toEqual([{
    method: 'POST',
    path: '/api/v1/carddav/conflicts/41/resolve',
    query: {},
    body: { choice: 'keep_local' }
  }]);

  await submitConflictChoice(page, 'Use msgvault version', 'Restore in address book');
  await expect(conflictRow).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'Unresolved conflicts' })).toBeFocused();
  expect(fixture.requests.filter(({ path }) => path === '/api/v1/carddav/conflicts/41/resolve')).toHaveLength(2);
  const requestLedger = JSON.stringify(fixture.requests);
  for (const marker of CARD_DAV_FORBIDDEN_MARKERS) expect(requestLedger).not.toContain(marker);
  await assertCardDAVForbiddenMarkersAbsent(page);
});

test('keyboard conflict resolution submits once and returns focus to the queue', async ({ page }) => {
  const fixture = await installCardDAV(page, { configured: true, staleConflictOnce: false });
  const release = fixture.holdNextConflictResolution();
  await openCardDAVSettings(page);

  const conflictRow = page.getByRole('button', { name: 'Review conflict 41 in Synthetic contacts' });
  await conflictRow.focus();
  await page.keyboard.press('Enter');
  const choice = page.getByRole('button', { name: 'Use address book version' }).first();
  await choice.focus();
  await page.keyboard.press('Enter');
  const dialog = page.getByRole('dialog', { name: 'Keep contact deleted' });
  const submit = dialog.getByRole('button', { name: 'Keep contact deleted' });
  await submit.focus();
  await page.keyboard.press('Enter');
  await expect.poll(() => mutationCount(fixture.requests, '/api/v1/carddav/conflicts/41/resolve')).toBe(1);

  release();
  await expect(dialog).toHaveCount(0);
  await expect(conflictRow).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'Unresolved conflicts' })).toBeFocused();
});

test('keyboard publication ambiguity locks mutation, retries GET only, and repeats a keyed handoff', async ({ page }) => {
  test.slow();
  await installMixedArchive(page);
  const fixture = await installCardDAV(page, { configured: true });
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({
    workspace: 'directory', directoryPersonID: 42
  }))}`);

  await page.getByRole('tab', { name: 'Maintenance' }).click();
  const publish = page.getByRole('switch', { name: 'Publish person to CardDAV' });
  await expect(publish).toBeVisible();
  await expect(page.getByText(/^Attributes:/)).toHaveCount(0);
  await publish.focus();
  await page.keyboard.press('Space');
  const unpublish = page.getByRole('switch', { name: 'Remove person from CardDAV' });
  await expect(unpublish).toBeFocused();
  await expect(page.getByRole('status', { name: 'Operation status' }))
    .toContainText('Published this person to CardDAV in Synthetic contacts.');
  expect(mutationCount(fixture.requests, '/api/v1/carddav/publications/42', 'POST')).toBe(1);

  fixture.failNextPublicationRead();
  await page.keyboard.press('Space');
  const publicationRegion = page.getByRole('region', { name: 'CardDAV publication' });
  await expect(publicationRegion.getByRole('alert'))
    .toContainText('Current CardDAV publication state is unknown.');
  await expect(unpublish).toHaveCount(0);
  await expect(publicationRegion.getByRole('switch')).toHaveCount(0);
  expect(mutationCount(fixture.requests, '/api/v1/carddav/publications/42', 'DELETE')).toBe(1);
  const readsBeforeRetry = mutationCount(fixture.requests, '/api/v1/carddav/publications/42', 'GET');

  const retry = page.getByRole('button', { name: 'Retry CardDAV publication state' });
  await retry.focus();
  await page.keyboard.press('Enter');
  const handoff = page.getByRole('button', { name: 'Review CardDAV conflict 42' });
  await expect(handoff).toBeVisible();
  expect(mutationCount(fixture.requests, '/api/v1/carddav/publications/42', 'GET')).toBe(readsBeforeRetry + 1);
  expect(mutationCount(fixture.requests, '/api/v1/carddav/publications/42', 'DELETE')).toBe(1);

  await handoff.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: 'Conflict comparison' })).toBeFocused();
  await expect(page.getByText('Resolved using the msgvault version.')).toBeVisible();
  expect(mutationCount(fixture.requests, '/api/v1/carddav/conflicts/42', 'GET')).toBe(1);
  await expect(page.getByRole('dialog')).toHaveCount(0);

  await page.goBack();
  await expect(page.getByRole('heading', { name: 'Archive Person' })).toBeVisible();
  await page.getByRole('tab', { name: 'Maintenance' }).click();
  const repeatedHandoff = page.getByRole('button', { name: 'Review CardDAV conflict 42' });
  await repeatedHandoff.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: 'Conflict comparison' })).toBeFocused();
  expect(mutationCount(fixture.requests, '/api/v1/carddav/conflicts/42', 'GET')).toBe(2);
  await assertCardDAVForbiddenMarkersAbsent(page);
});

async function openCardDAVSettings(page: Page): Promise<void> {
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings' }))}`);
  const category = page.getByRole('button', { name: /^CardDAV account/ });
  await category.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: 'CardDAV account' })).toBeVisible();
}

function mutationCount(
  requests: Array<{ method: string; path: string }>,
  path: string,
  method = 'POST'
): number {
  return requests.filter((request) => request.path === path && request.method === method).length;
}

async function submitConflictChoice(page: Page, label: string, confirmationLabel: string): Promise<void> {
  const choice = page.getByRole('button', { name: label }).first();
  await choice.focus();
  await page.keyboard.press('Enter');
  const dialog = page.getByRole('dialog', { name: confirmationLabel });
  await expect(dialog).toBeVisible();
  const submit = dialog.getByRole('button', { name: confirmationLabel });
  await submit.focus();
  await page.keyboard.press('Enter');
  await expect(dialog).toHaveCount(0);
}

test('scheduled CardDAV queue stays visible through admission and completion', async ({ page }) => {
  await installCardDAV(page, { configured: true });
  await page.clock.install({ time: new Date('2026-10-01T15:00:00Z') });
  let stage: 'idle' | 'queued' | 'admitted' | 'completed' = 'idle';
  const completed = {
    id: 301, account_id: 1, connection: 'default', trigger: 'scheduled', full: false,
    state: 'succeeded', started_at: '2026-10-01T15:01:00Z', finished_at: '2026-10-01T15:02:00Z',
    books: 1, created: 1, updated: 0, removed: 0,
  };
  await page.route('**/api/v1/carddav/status**', (route) => route.fulfill({ json: {
    configured: true, available: true, credential_configured: true, enabled: true,
    scheduled: true, schedule: '0 * * * *', next_scheduled_at: '2026-10-01T16:00:00Z',
    scheduler_running: stage === 'admitted',
    ...(stage === 'queued' ? { queued_at: '2026-10-01T15:00:30Z' } : {}),
    ...(stage === 'completed' ? { latest: completed } : {}),
  } }));
  await page.route('**/api/v1/carddav/books**', (route) => route.fulfill({ json: {
    books: stage === 'completed' ? [{ id: 301, name: 'Refreshed contacts', connection: 'default', account_id: 1, subscribed: true, lookup_source: false, write_target: false, needs_full_reconcile: false }] : [],
  } }));
  await page.route('**/api/v1/carddav/runs**', (route) => route.fulfill({ json: { runs: stage === 'completed' ? [completed] : [] } }));
  await page.goto(`/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'settings', settingsCategory: 'carddav' }))}`);
  await expect(page.getByText('CardDAV status', { exact: true })).toBeVisible();
  await expect(page.getByText('No discovered address books.', { exact: true })).toBeVisible();
  stage = 'queued';
  await page.clock.runFor(30_000);
  await expect(page.getByText('Queued since', { exact: false })).toBeVisible();
  stage = 'admitted';
  await page.clock.runFor(500);
  await expect(page.getByLabel('Active CardDAV sync')).toHaveText('Running');
  await expect(page.getByText('Queued since', { exact: false })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Sync now', exact: true })).toBeDisabled();
  stage = 'completed';
  await page.clock.runFor(500);
  await expect(page.getByText('Refreshed contacts', { exact: true })).toBeVisible();
  await expect(page.getByRole('table', { name: 'CardDAV sync history' }).getByRole('row')).toHaveCount(2);
});
