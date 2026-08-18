import { expect, test, type Page } from '@playwright/test'

const now = '2026-07-20T00:00:00Z'
const releaseSource = 'NousResearch/hermes-agent GitHub Releases'
const releases = [
  { version: '0.19.0', tag: 'v2026.7.20', commit: '3ef6bbd201263d354fd83ec55b3c306ded2eb72a', image: 'local/hermes-fleet-runtime:0.19.0-3ef6bbd20126', url: 'https://github.com/NousResearch/hermes-agent/releases/tag/v2026.7.20', published_at: now },
  { version: '0.18.2', tag: 'v2026.7.7.2', commit: '9de9c25f620ff7f1ce0fd5457d596052d5159596', image: 'local/hermes-fleet-runtime:0.18.2-9de9c25f620f', url: 'https://github.com/NousResearch/hermes-agent/releases/tag/v2026.7.7.2', published_at: now },
  { version: '0.18.1', tag: 'v2026.7.7', commit: 'f9eca7e15f1c2bfe5194aae5aa489af53c0a1a23', image: 'local/hermes-fleet-runtime:0.18.1-f9eca7e15f1c', url: 'https://github.com/NousResearch/hermes-agent/releases/tag/v2026.7.7', published_at: now },
]
const host = {
  id: 'host-1',
  name: 'local-mac',
  hostname: 'local-mac',
  os: 'darwin',
  arch: 'arm64',
  agent_version: '0.10.0',
  status: 'ONLINE',
  last_seen_at: now,
  created_at: now,
}

async function openFleet(page: Page, instances: unknown[] = [], operations: unknown[] = []) {
  await page.addInitScript(() => sessionStorage.setItem('fleet-admin-token', 'e2e-token'))
  await page.route('**/api/v1/events', async (route) => {
    await route.fulfill({ status: 200, contentType: 'text/event-stream', body: '' })
  })
  await page.route('**/api/v1/overview', async (route) => {
    await route.fulfill({ json: { hosts: [host], instances, operations } })
  })
  await page.route(/\/api\/v1\/operations(?:\?.*)?$/, async (route) => {
    await route.fulfill({ json: { items: operations, next_cursor: null } })
  })
  await page.route('**/api/v1/hermes-releases', async (route) => {
    await route.fulfill({ json: { source: releaseSource, checked_at: now, releases } })
  })
  await page.route('**/api/v1/instances/*/hermes-update', async (route) => {
    await route.fulfill({ json: {
      current_image: 'local/hermes-fleet-runtime:0.19.0', target_version: '0.19.0',
      target_source: '7acaff5ef2bcbaa22bd23b72efe60906123a4f55', target_image: 'local/hermes-fleet-runtime:0.18.2',
      official_status: 'CURRENT', update_kind: 'NONE', official_source: releaseSource, official_checked_at: now, latest_release: releases[0],
      available: false, eligible: false, reason: 'No newer installable Hermes version is available',
    } })
  })
  await page.goto('/')
  await page.getByRole('button', { name: 'Instances', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Instances', level: 1 })).toBeVisible()
}

test('create instance exposes only operator choices and locks while submitting', async ({ page }) => {
  let requestBody: unknown
  let releaseRequest!: () => void
  const requestHeld = new Promise<void>((resolve) => { releaseRequest = resolve })
  await page.route('**/api/v1/instances', async (route) => {
    requestBody = route.request().postDataJSON()
    await requestHeld
    await route.fulfill({ status: 202, json: { id: 'operation-1' } })
  })
  await openFleet(page)

  await page.getByRole('button', { name: 'Create instance' }).click()
  const dialog = page.getByRole('dialog', { name: 'Create instance' })
  await expect(dialog.getByLabel('Instance name')).toBeVisible()
	await expect(dialog.getByLabel('Instance name')).toHaveAttribute('pattern', '[a-z](?:[a-z0-9]|-){2,31}')
	await expect(dialog.getByLabel('Instance name').evaluate((input) => {
		try {
			new RegExp((input as HTMLInputElement).pattern, 'v')
			return true
		} catch {
			return false
		}
	})).resolves.toBe(true)
  await expect(dialog.getByLabel('Host')).toBeVisible()
  await expect(dialog.getByLabel('Provider')).toHaveValue('openai-codex')
  await expect(dialog.getByLabel('Hermes version')).toHaveValue('0.19.0')
  await expect(dialog.getByLabel('Hermes version').locator('option')).toHaveCount(3)
  await expect(dialog.getByText(/Model|Reasoning|API port|Dashboard port/)).toHaveCount(0)

  await dialog.getByLabel('Instance name').fill('fleet-e2e-01')
  await dialog.getByRole('button', { name: 'Create instance' }).click()
  await expect(dialog.getByRole('button', { name: 'Creating' })).toBeDisabled()
  await expect(dialog.getByLabel('Instance name')).toBeDisabled()
  await expect.poll(() => requestBody).toEqual({ name: 'fleet-e2e-01', host_id: 'host-1', hermes_version: '0.19.0', provider: 'openai-codex' })

  releaseRequest()
  await expect(dialog).toBeHidden()
})

test('System keeps Fleet Manager settings, remote access, and backups outside instance configuration', async ({ page }) => {
	const instance = {
		id: 'instance-1', name: 'fleet-test-01', host_id: host.id, host_name: host.name, status: 'RUNNING',
		image: 'local/hermes-fleet-runtime:0.18.2', image_id: `sha256:${'a'.repeat(64)}`, provider: 'openai-codex', model: 'gpt-5.6-sol',
		reasoning: 'medium', service_tier: 'normal', codex_configured: true, api_port: 8650, dashboard_port: 9130,
		project_name: 'hermes-fleet-test', data_volume: 'hermes-fleet-test-data', managed_path: '/tmp/hermes-fleet-test',
		created_at: now, updated_at: now,
		observation: { instance_id: 'instance-1', hermes_version: '0.18.2', hermes_source: '7acaff5ef2bcbaa22bd23b72efe60906123a4f55', model_catalog: ['gpt-5.6-sol', 'gpt-5.6-terra'], recommended_model: 'gpt-5.6-sol', status: 'IN_SYNC', summary: 'Healthy', received_at: now, checks: [
			{ name: 'codex_auth', status: 'OK', detail: 'Codex authentication is connected' },
			{ name: 'runtime_configuration', status: 'OK', detail: 'Hermes configuration matches Fleet' },
		] },
	}
	await page.route('**/api/v1/system', async (route) => route.fulfill({ json: {
		fleet_version: '0.10.0', build_id: 'test-build', operator_url: 'http://127.0.0.1:9180', database_path: '/var/lib/hermes-fleet/fleet.db', backup_retention: 20,
		remote_access: { configured: false, state: 'disabled', admin: { routes: 0, synced: false }, instances: { routes: 0, synced: false } },
	} }))
	await page.route('**/api/v1/backups', async (route) => route.fulfill({ json: [] }))
	await page.route('**/api/v1/instances/instance-1/recovery-points', async (route) => route.fulfill({ json: [] }))
	await openFleet(page, [instance])
	await expect(page.getByRole('columnheader', { name: 'Hermes' })).toBeVisible()
	await expect(page.getByRole('columnheader', { name: 'Configuration' })).toHaveCount(0)
	await expect(page.getByText('0.18.2')).toBeVisible()
	await expect(page.getByText('Source commit 7acaff5ef2bc')).toHaveCount(0)

	await page.getByRole('button', { name: instance.name }).click()
	await expect(page.getByText('Hermes version')).toBeVisible()
	await expect(page.getByText('Latest version installed')).toBeVisible()
	await expect(page.getByRole('link', { name: /GitHub Releases/ })).toBeVisible()
	await page.getByRole('button', { name: 'Provider' }).click()
	await expect(page.getByRole('heading', { name: 'Providers', exact: true })).toBeVisible()
	await expect(page.getByRole('heading', { name: 'Codex configuration', exact: true })).toBeVisible()
	await expect(page.getByLabel('Model')).toHaveValue('gpt-5.6-sol')
	await expect(page.getByLabel('Reasoning')).toHaveValue('medium')
	await expect(page.getByLabel('Service tier')).toHaveValue('normal')
	await expect(page.getByRole('button', { name: 'Backups' })).toBeVisible()

	await page.getByRole('button', { name: 'System' }).click()
	await expect(page).toHaveURL(/#system\/general$/)
	await expect(page.getByRole('heading', { name: 'System overview' })).toBeVisible()
	await expect(page.getByText('Fleet Manager', { exact: true })).toBeVisible()
	await expect(page.getByText('Version 0.10.0')).toBeVisible()
	await expect(page.getByText('Host Agent compatibility')).toHaveCount(0)
	await page.getByText('Technical details', { exact: true }).click()
	await expect(page.getByText('http://127.0.0.1:9180')).toBeVisible()
	await expect(page.getByText('/var/lib/hermes-fleet/fleet.db')).toBeVisible()
	await expect(page.getByText('gpt-5.6-sol')).toHaveCount(0)
	await expect(page.getByText('New instance defaults')).toHaveCount(0)
	await expect(page.getByRole('button', { name: 'Run recovery drill' })).toHaveCount(0)

	await page.getByRole('button', { name: 'Backups & recovery' }).click()
	await expect(page).toHaveURL(/#system\/backups$/)
	await expect(page.getByRole('heading', { name: 'Backups & recovery' })).toBeVisible()
	await expect(page.getByText('Fleet database only')).toBeVisible()
	await expect(page.getByRole('button', { name: 'Run recovery drill' })).toBeVisible()
})

test('Hermes update queues one persistent Host Agent workflow', async ({ page }) => {
	const instance = {
		id: 'instance-update', name: 'fleet-update-01', host_id: host.id, host_name: host.name, status: 'STOPPED',
		image: 'local/hermes-fleet-runtime:0.18.2', image_id: `sha256:${'a'.repeat(64)}`, provider: 'openai-codex',
		model: 'gpt-5.6-sol', reasoning: 'medium', service_tier: 'normal', codex_configured: true,
		api_port: 8650, dashboard_port: 9130, project_name: 'hermes-fleet-update', data_volume: 'hermes-fleet-update-data',
		managed_path: '/tmp/hermes-fleet-update', created_at: now, updated_at: now,
		observation: {
			instance_id: 'instance-update', hermes_version: '0.18.2',
			hermes_source: '7acaff5ef2bcbaa22bd23b72efe60906123a4f55', status: 'IN_SYNC', summary: 'Healthy', received_at: now,
			checks: [],
		},
	}
	const updateBackup = {
		id: `recovery-${'c'.repeat(32)}`, instance_id: instance.id, instance_name: instance.name,
		filename: 'fleet-update-01-backup.tar', status: 'READY', size_bytes: 1024, encrypted_size_bytes: 1088,
		image: instance.image, image_id: instance.image_id, provider: instance.provider, model: instance.model,
		reasoning: instance.reasoning, service_tier: instance.service_tier, codex_configured: true,
		project_name: instance.project_name, data_volume: instance.data_volume, agent_version: host.agent_version,
		created_at: now, verified_at: now,
	}
	await page.route('**/api/v1/instances/instance-update/recovery-points', async (route) => {
		await route.fulfill({ json: [updateBackup] })
	})
	await openFleet(page, [instance])
	let updateRequest: unknown
	await page.route('**/api/v1/instances/instance-update/hermes-update', async (route) => {
		if (route.request().method() === 'POST') {
			updateRequest = route.request().postDataJSON()
			await route.fulfill({ status: 202, json: {
				id: 'upgrade-1', instance_id: instance.id, workflow_id: '00000000-0000-4000-8000-000000000099',
				type: 'UPGRADE_HERMES', status: 'RUNNING', progress: { stage: 'INSTALLING' },
				metadata: { from_version: '0.18.2', to_version: '0.19.0', original_status: 'STOPPED', update_kind: 'VERSION_UPDATE' },
				summary: 'Update Hermes fleet-update-01 to 0.19.0', created_at: now, updated_at: now,
			} })
			return
		}
		await route.fulfill({ json: {
			current_version: '0.18.2', current_source: instance.observation.hermes_source, current_image: instance.image,
			official_status: 'UPDATE_AVAILABLE', update_kind: 'VERSION_UPDATE', official_source: releaseSource, official_checked_at: now,
			latest_release: { ...releases[0], version: '0.19.0', image: 'local/hermes-fleet-runtime:0.19.0', url: 'https://github.com/NousResearch/hermes-agent/releases/tag/v2026.8.1' },
			target_version: '0.19.0', target_source: '8bcdef6ef2bcbaa22bd23b72efe60906123a4f66',
			target_image: 'local/hermes-fleet-runtime:0.19.0', available: true, eligible: true,
			reason: 'Fleet will prepare the release, create a verified backup, update Hermes, and restore the current runtime state',
		} })
	})

	await page.getByRole('button', { name: instance.name }).click()
	await expect(page.getByText('Update available: 0.19.0')).toBeVisible()
	page.once('dialog', async (dialog) => {
		expect(dialog.message()).toContain('create and verify a rollback backup')
		expect(dialog.message()).toContain('Progress remains visible on this page')
		await dialog.accept()
	})
	await page.getByRole('button', { name: 'Update to 0.19.0' }).click()
	await expect(page.getByText('Install and verify Hermes')).toBeVisible()
	await expect(page.locator('.hermes-update-steps [aria-current="step"]')).toContainText('Install and verify Hermes')
	await expect.poll(() => updateRequest).not.toBeUndefined()
	const updateBody = updateRequest as Record<string, string>
	expect(updateBody).toMatchObject({ confirm_name: instance.name })
	expect(updateBody.backup_id).toBeUndefined()
	expect(updateBody.workflow_id).toMatch(/^[a-f0-9-]{36}$/)
	await page.getByLabel('Instance modules').getByRole('button', { name: 'Operations', exact: true }).click()
	await expect(page.getByText(/current step: installing Hermes/i)).toBeVisible()
})

test('same-version runtime maintenance stays separate from Hermes update information', async ({ page }) => {
	const instance = {
		id: 'instance-refresh', name: 'fleet-refresh-01', host_id: host.id, host_name: host.name, status: 'RUNNING',
		image: `${releases[0].image}-111111111111`, image_id: `sha256:${'a'.repeat(64)}`, provider: 'openai-codex',
		model: 'gpt-5.6-sol', reasoning: 'medium', service_tier: 'normal', codex_configured: true,
		api_port: 8650, dashboard_port: 9130, project_name: 'hermes-fleet-refresh', data_volume: 'hermes-fleet-refresh-data',
		managed_path: '/tmp/hermes-fleet-refresh', created_at: now, updated_at: now,
		observation: {
			instance_id: 'instance-refresh', hermes_version: releases[0].version,
			hermes_source: releases[0].commit, status: 'IN_SYNC', summary: 'Healthy', received_at: now, checks: [],
		},
	}
	await page.route('**/api/v1/instances/instance-refresh/recovery-points', async (route) => {
		await route.fulfill({ json: [] })
	})
	await openFleet(page, [instance])
	let maintenanceRequest: unknown
	await page.route('**/api/v1/instances/instance-refresh/hermes-update', async (route) => {
		if (route.request().method() === 'POST') {
			maintenanceRequest = route.request().postDataJSON()
			await route.fulfill({ status: 202, json: {
				id: 'refresh-1', instance_id: instance.id, workflow_id: '00000000-0000-4000-8000-000000000199',
				type: 'UPGRADE_HERMES', status: 'RUNNING', progress: { stage: 'INSTALLING' },
				metadata: {
					from_version: releases[0].version, to_version: releases[0].version,
					original_status: 'RUNNING', update_kind: 'RUNTIME_REFRESH',
				},
				summary: 'Refresh managed Hermes runtime for fleet-refresh-01', created_at: now, updated_at: now,
			} })
			return
		}
		await route.fulfill({ json: {
			current_version: releases[0].version, current_source: releases[0].commit, current_image: instance.image,
			official_status: 'CURRENT', update_kind: 'RUNTIME_REFRESH',
			official_source: releaseSource, official_checked_at: now, latest_release: releases[0],
			target_version: releases[0].version, target_source: releases[0].commit,
			target_image: `${releases[0].image}-222222222222`, available: true, eligible: true,
			reason: 'Hermes remains on the same version while Fleet refreshes its managed runtime',
		} })
	})

	await page.getByRole('button', { name: instance.name }).click()
	await expect(page.getByText('Latest version installed')).toBeVisible()
	await expect(page.getByText(/Update available:/)).toHaveCount(0)
	await expect(page.getByText('Managed runtime refresh required')).toBeVisible()
	await expect(page.getByText(`Hermes ${releases[0].version} remains installed.`)).toBeVisible()
	await expect(page.getByRole('button', { name: 'Complete setup' })).toHaveCount(0)
	page.once('dialog', async (dialog) => {
		expect(dialog.message()).toContain(`Hermes ${releases[0].version} will remain installed`)
		expect(dialog.message()).not.toContain(`from Hermes ${releases[0].version} to ${releases[0].version}`)
		await dialog.accept()
	})
	await page.getByRole('button', { name: 'Refresh managed runtime' }).click()
	await expect.poll(() => maintenanceRequest).not.toBeUndefined()

	await page.getByLabel('Instance modules').getByRole('button', { name: 'Overview', exact: true }).click()
	await expect(page.getByText(`Refreshing managed runtime for Hermes ${releases[0].version}`)).toBeVisible()
	await expect(page.getByText('Refresh managed runtime')).toBeVisible()
	await expect(page.locator('.hermes-update-steps [aria-current="step"]')).toContainText('Refresh managed runtime')

	await page.getByLabel('Instance modules').getByRole('button', { name: 'Operations', exact: true }).click()
	await expect(page.getByText(`Hermes ${releases[0].version} unchanged`)).toBeVisible()
	await expect(page.getByText(`${releases[0].version} → ${releases[0].version}`)).toHaveCount(0)
})

test('Hermes update is recorded as one durable operation', async ({ page }) => {
	const workflowID = '00000000-0000-4000-8000-000000000021'
	const operations = [
		{ id: 'operation-update', instance_id: 'instance-update', workflow_id: workflowID, actor: 'FLEET_ADMIN', type: 'UPGRADE_HERMES', status: 'SUCCEEDED', summary: 'Update Hermes fleet-update-01 to 0.19.0', metadata: { from_version: '0.18.2', to_version: '0.19.0', update_kind: 'VERSION_UPDATE', backup_id: 'backup-12345678', attempt: 1 }, created_at: '2026-07-20T00:01:00Z', updated_at: '2026-07-20T00:01:20Z' },
		{ id: 'operation-auth', instance_id: 'instance-update', actor: 'FLEET_ADMIN', type: 'CODEX_AUTH', status: 'SUCCEEDED', summary: 'Authenticate Codex fleet-update-01', metadata: { attempt: 1 }, created_at: '2026-07-19T23:50:00Z', updated_at: '2026-07-19T23:50:10Z' },
	]
	await openFleet(page, [], operations)
	await page.getByRole('button', { name: 'Operations' }).click()
	await expect(page.getByText('2 operations shown')).toBeVisible()
	const updateRow = page.getByRole('row').filter({ hasText: 'Update Hermes fleet-update-01 to 0.19.0' })
	await expect(updateRow).toBeVisible()
	await expect(updateRow.getByText('Succeeded')).toBeVisible()
})

test('unconfigured instance shows authentication before Codex settings', async ({ page }) => {
  const instance = {
    id: 'instance-pending', name: 'fleet-pending-01', host_id: host.id, host_name: host.name, status: 'RUNNING',
    image: 'hermes:test', image_id: `sha256:${'a'.repeat(64)}`, provider: 'openai-codex', model: 'gpt-5.6-sol', reasoning: 'medium', service_tier: 'normal',
    codex_configured: false,
    api_port: 8650, dashboard_port: 9130, project_name: 'hermes-fleet-pending', data_volume: 'hermes-fleet-pending-data',
    managed_path: '/tmp/hermes-fleet-pending', created_at: now, updated_at: now,
	    observation: { instance_id: 'instance-pending', status: 'DEGRADED', summary: 'Codex sign-in required', received_at: now, checks: [
	      { name: 'codex_auth', status: 'DRIFT', detail: 'No Codex credentials stored' },
	      { name: 'provider_auth', status: 'DRIFT', detail: 'Grok authentication is not connected' },
	      { name: 'runtime_configuration', status: 'DRIFT', detail: 'Choose a Codex model in Hermes Fleet' },
    ] },
  }
  await page.route('**/api/v1/instances/instance-pending/recovery-points', async (route) => route.fulfill({ json: [] }))
  await openFleet(page, [instance])

  await page.getByRole('button', { name: instance.name }).click()
	await expect(page.getByRole('button', { name: 'Overview', exact: true })).toBeVisible()
	await expect(page.getByText('Complete Codex setup')).toBeVisible()
	await page.getByRole('button', { name: 'Diagnostics', exact: true }).click()
	await expect(page.getByText('0 issues · 1 setup item · 0 passed')).toBeVisible()
	await expect(page.getByRole('cell', { name: 'Codex setup' })).toBeVisible()
	await expect(page.getByRole('cell', { name: 'Setup incomplete' })).toHaveCount(1)
  await page.getByRole('button', { name: 'Provider', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Providers', exact: true })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Codex configuration' })).toBeVisible()
  await expect(page.getByText('Not configured')).toHaveCount(4)
  await expect(page.getByText('gpt-5.6-sol')).toHaveCount(0)
  await expect(page.getByLabel('Model')).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Authenticate Codex' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Authenticate Grok' })).toBeVisible()
	  await expect(page.getByRole('button', { name: 'Configure as default' })).toHaveCount(1)
})

test('connected Codex does not turn legacy defaults into a saved configuration', async ({ page }) => {
  const instance = {
    id: 'instance-pending-connected', name: 'fleet-pending-connected', host_id: host.id, host_name: host.name, status: 'RUNNING',
    image: 'hermes:test', image_id: `sha256:${'a'.repeat(64)}`, provider: 'openai-codex', model: 'gpt-5.6-sol',
    reasoning: 'medium', service_tier: 'normal', codex_configured: false, api_port: 8650, dashboard_port: 9130,
    project_name: 'hermes-fleet-pending-connected', data_volume: 'hermes-fleet-pending-connected-data',
    managed_path: '/tmp/hermes-fleet-pending-connected',
    last_error: 'runtime synchronization refused: model contains unsupported characters or length', created_at: now, updated_at: now,
    observation: { instance_id: 'instance-pending-connected', model_catalog: ['gpt-5.6-sol', 'gpt-5.6-terra'], recommended_model: 'gpt-5.6-sol', status: 'DEGRADED', summary: 'Codex configuration required', received_at: now, checks: [
      { name: 'codex_auth', status: 'OK', detail: 'Codex authentication is connected' },
      { name: 'runtime_configuration', status: 'DRIFT', detail: 'Codex configuration has not been saved in Hermes Fleet' },
    ] },
  }
  await page.route('**/api/v1/instances/instance-pending-connected/recovery-points', async (route) => route.fulfill({ json: [] }))
  await openFleet(page, [instance])
	await expect(page.getByText('Previous action failed')).toHaveCount(0)
	await expect(page.getByText('Error recorded')).toHaveCount(0)
	await expect(page.getByRole('button', { name: 'View status details: Setup incomplete' })).toBeVisible()

  await page.getByRole('button', { name: instance.name }).click()
	await expect(page.getByRole('button', { name: 'Overview', exact: true })).toBeVisible()
	await expect(page.getByText('Signed in', { exact: true })).toBeVisible()
	await expect(page.getByText('Model not configured')).toBeVisible()
	await expect(page.getByText('Choose a Codex model')).toBeVisible()
  await page.getByRole('button', { name: 'Provider', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Configure Codex' })).toBeVisible()
	await expect(page.getByLabel('Model')).toHaveValue('gpt-5.6-sol')
	await expect(page.getByLabel('Model').locator('option')).toHaveCount(3)
	await expect(page.getByLabel('Model').locator('option').filter({ hasText: '· recommended' })).toHaveCount(1)
	await expect(page.getByText('Recommended by Hermes on this instance')).toHaveCount(0)
  await expect(page.getByLabel('Reasoning')).toHaveValue('medium')
  await expect(page.getByLabel('Service tier')).toHaveValue('normal')
})

test('failed Codex authentication preserves and identifies an existing configuration', async ({ page }) => {
  const instance = {
    id: 'instance-1',
    name: 'fleet-test-01',
    host_id: host.id,
    host_name: host.name,
    status: 'RUNNING',
    image: 'hermes:test',
    image_id: `sha256:${'a'.repeat(64)}`,
    provider: 'openai-codex',
    model: 'gpt-5.6-sol',
    reasoning: 'medium',
    service_tier: 'normal',
    codex_configured: true,
    api_port: 8650,
    dashboard_port: 9130,
    project_name: 'hermes-fleet-test',
    data_volume: 'hermes-fleet-test-data',
    managed_path: '/tmp/hermes-fleet-test',
    created_at: now,
    updated_at: now,
    observation: {
      instance_id: 'instance-1',
      status: 'DEGRADED',
      summary: 'Codex sign-in required',
      received_at: now,
      checks: [{ name: 'codex_auth', status: 'DRIFT', detail: 'No Codex credentials stored' }],
    },
  }
  await page.route('**/api/v1/instances/instance-1/recovery-points', async (route) => {
    await route.fulfill({ json: [] })
  })
  await page.route('**/api/v1/instances/instance-1/codex-auth', async (route) => {
    await route.fulfill({ status: 202, json: {
	      operation_id: 'auth-1', instance_id: 'instance-1', provider: 'openai-codex', status: 'RUNNING', stage: 'AWAITING_USER',
      verification_uri: 'https://example.test/device', user_code: 'STALE-CODE', expires_at: '2026-07-20T01:00:00Z',
      created_at: now, updated_at: now,
    } })
  })
  await page.route('**/api/v1/instances/instance-1/codex-auth/auth-1', async (route) => {
    await route.fulfill({ json: {
	      operation_id: 'auth-1', instance_id: 'instance-1', provider: 'openai-codex', status: 'FAILED', stage: 'AWAITING_USER',
      verification_uri: 'https://example.test/device', user_code: 'STALE-CODE',
      error: 'Device authorization expired', created_at: now, updated_at: now,
    } })
  })
  await openFleet(page, [instance])

  await page.getByRole('button', { name: 'fleet-test-01' }).click()
	await page.getByRole('button', { name: 'Provider', exact: true }).click()
	await expect(page.getByRole('heading', { name: 'Codex configuration' })).toBeVisible()
	await expect(page.getByText('Configuration saved, Codex not connected')).toBeVisible()
	await expect(page.getByText('gpt-5.6-sol')).toBeVisible()
  await page.getByRole('button', { name: 'Authenticate Codex' }).click()
  const dialog = page.getByRole('dialog', { name: 'Authenticate Codex' })
  await expect(dialog.getByText('Device authorization expired')).toBeVisible()
  await expect(dialog.getByText('STALE-CODE')).toHaveCount(0)
  await expect(dialog.getByRole('link', { name: 'Open OpenAI sign-in' })).toHaveCount(0)
})

test('credential reveal tracks the server operation until credentials are ready', async ({ page }) => {
  const instance = {
    id: 'instance-1', name: 'fleet-test-01', host_id: host.id, host_name: host.name, status: 'RUNNING',
    image: 'hermes:test', image_id: `sha256:${'a'.repeat(64)}`, provider: 'openai-codex', model: 'gpt-5.6-sol',
    reasoning: 'medium', service_tier: 'normal', codex_configured: true, api_port: 8650, dashboard_port: 9130,
    project_name: 'hermes-fleet-test', data_volume: 'hermes-fleet-test-data', managed_path: '/tmp/hermes-fleet-test',
    created_at: now, updated_at: now,
  }
  let polls = 0
  await page.route('**/api/v1/instances/instance-1/recovery-points', async (route) => route.fulfill({ json: [] }))
  await page.route('**/api/v1/instances/instance-1/credentials', async (route) => route.fulfill({
    status: 202,
    json: { id: 'reveal-1', instance_id: 'instance-1', type: 'CREDENTIAL_REVEAL', status: 'PENDING', summary: 'Reveal credentials', created_at: now, updated_at: now },
  }))
  await page.route('**/api/v1/credential-reveals/reveal-1', async (route) => {
    polls += 1
    if (polls < 3) {
      await route.fulfill({ status: 202, json: { id: 'reveal-1', instance_id: 'instance-1', type: 'CREDENTIAL_REVEAL', status: polls === 1 ? 'PENDING' : 'RUNNING', summary: 'Reveal credentials', created_at: now, updated_at: now } })
      return
    }
    await route.fulfill({ json: { credentials: { dashboard_username: 'admin', dashboard_password: 'secret', api_server_key: 'key' }, expires_at: '2099-01-01T00:00:00Z' } })
  })
  await openFleet(page, [instance])

  await page.getByRole('button', { name: 'fleet-test-01' }).click()
	await page.getByRole('button', { name: 'Access' }).click()
	await page.getByRole('button', { name: 'Reveal credentials' }).click()
	await expect(page.getByRole('button', { name: /Queued|Reading/ })).toBeDisabled()
	await expect(page.getByText('admin', { exact: true })).toBeVisible({ timeout: 10000 })
	expect(polls).toBe(3)
})

test('backup restore requires exact instance-name confirmation', async ({ page }) => {
  const instance = {
    id: 'instance-1', name: 'fleet-test-01', host_id: host.id, host_name: host.name, status: 'STOPPED',
    image: 'hermes:test', image_id: `sha256:${'a'.repeat(64)}`, provider: 'openai-codex', model: 'gpt-5.6-sol',
    reasoning: 'medium', service_tier: 'normal', codex_configured: true, api_port: 8650, dashboard_port: 9130,
    project_name: 'hermes-fleet-test', data_volume: 'hermes-fleet-test-data', managed_path: '/tmp/hermes-fleet-test',
    created_at: now, updated_at: now,
  }
  const point = {
    id: 'recovery-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', instance_id: instance.id, instance_name: instance.name,
    filename: 'hermes-fleet-test-01-20260720T000000Z-aaaaaaaa.tar', status: 'READY', size_bytes: 4096,
    sha256: 'b'.repeat(64), image: instance.image, image_id: instance.image_id, provider: instance.provider,
    model: instance.model, reasoning: instance.reasoning, service_tier: instance.service_tier, codex_configured: instance.codex_configured,
    project_name: instance.project_name, data_volume: instance.data_volume, managed_path: instance.managed_path,
    agent_version: host.agent_version, created_at: now, verified_at: now,
  }
  let restoreBody: unknown
  let restoreReads = 0
  await page.route('**/api/v1/instances/instance-1/recovery-points', async (route) => route.fulfill({ json: [point] }))
  await page.route('**/api/v1/recovery-points/recovery-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/restore', async (route) => {
    restoreBody = route.request().postDataJSON()
    await route.fulfill({ status: 202, json: { id: 'restore-1', instance_id: instance.id, type: 'RESTORE', status: 'PENDING', summary: 'Restore from point', created_at: now, updated_at: now } })
  })
  await page.route('**/api/v1/operations/restore-1', async (route) => {
    restoreReads += 1
    await route.fulfill({ json: { id: 'restore-1', instance_id: instance.id, type: 'RESTORE', status: 'SUCCEEDED', summary: 'Restore from point', created_at: now, updated_at: now } })
  })
  page.on('dialog', async (dialog) => dialog.accept(instance.name))
  await openFleet(page, [instance])

  await page.getByRole('button', { name: instance.name }).click()
	await page.getByRole('button', { name: 'Backups' }).click()
	await page.getByTitle('Restore this backup').click()
  await expect.poll(() => restoreBody).toEqual({ confirm_name: instance.name })
  await expect.poll(() => restoreReads).toBe(1)
})

test('incomplete Hermes setup exposes the Fleet-owned automatic fix', async ({ page }) => {
  const instance = {
    id: 'instance-1',
    name: 'fleet-test-01',
    host_id: host.id,
    host_name: host.name,
    status: 'RUNNING',
    image: 'hermes:test',
    image_id: `sha256:${'a'.repeat(64)}`,
    provider: 'openai-codex',
    model: 'gpt-5.6-sol',
    reasoning: 'medium',
    service_tier: 'normal',
    codex_configured: true,
    api_port: 8650,
    dashboard_port: 9130,
    project_name: 'hermes-fleet-test',
    data_volume: 'hermes-fleet-test-data',
    managed_path: '/tmp/hermes-fleet-test',
    created_at: now,
    updated_at: now,
    observation: {
      instance_id: 'instance-1',
      status: 'DEGRADED',
      summary: 'Runtime drift detected',
      received_at: now,
      checks: [
        { name: 'runtime_configuration', status: 'DRIFT', detail: 'Hermes has not applied the Fleet provider and model' },
        { name: 'codex_auth', status: 'OK', detail: 'Codex authentication is connected' },
      ],
    },
  }
  let requestBody: unknown
  let operationReads = 0
  await page.route('**/api/v1/instances/instance-1/recovery-points', async (route) => {
    await route.fulfill({ json: [] })
  })
  await page.route('**/api/v1/instances/instance-1/actions', async (route) => {
    requestBody = route.request().postDataJSON()
    await route.fulfill({ status: 202, json: {
      id: 'sync-1', instance_id: instance.id, type: 'SYNC_RUNTIME', status: 'PENDING',
      summary: 'Complete Hermes setup', created_at: now, updated_at: now,
    } })
  })
  await page.route('**/api/v1/operations/sync-1', async (route) => {
    operationReads += 1
    await route.fulfill({ json: {
      id: 'sync-1', instance_id: instance.id, type: 'SYNC_RUNTIME', status: 'SUCCEEDED',
      summary: 'Complete Hermes setup', created_at: now, updated_at: now,
    } })
  })
  await openFleet(page, [instance])

  await page.getByRole('button', { name: 'fleet-test-01' }).click()
  await expect(page.getByText('Apply Codex configuration')).toBeVisible()
  await page.getByRole('button', { name: 'Complete setup' }).click()
  await expect.poll(() => requestBody).toEqual({ action: 'sync-runtime', confirm_name: 'fleet-test-01' })
  await expect.poll(() => operationReads).toBe(1)
})

test('runtime drift exposes a managed restart that remains visible until verification completes', async ({ page }) => {
  const instance = {
    id: 'instance-1',
    name: 'fleet-test-01',
    host_id: host.id,
    host_name: host.name,
    status: 'RUNNING',
    image: 'hermes:test',
    image_id: `sha256:${'a'.repeat(64)}`,
    provider: 'openai-codex',
    model: 'gpt-5.6-sol',
    reasoning: 'medium',
    service_tier: 'normal',
    codex_configured: true,
    api_port: 8650,
    dashboard_port: 9130,
    project_name: 'hermes-fleet-test',
    data_volume: 'hermes-fleet-test-data',
    managed_path: '/tmp/hermes-fleet-test',
    created_at: now,
    updated_at: now,
    observation: {
      instance_id: 'instance-1',
      status: 'DEGRADED',
      summary: 'Runtime drift detected',
      received_at: now,
      checks: [
        { name: 'runtime', status: 'DRIFT', detail: 'Desired RUNNING state does not match container state or health' },
        { name: 'image', status: 'OK', detail: 'Container image IDs match the provisioned image' },
      ],
    },
  }
  const operations: unknown[] = []
  let requestBody: unknown
  await page.route('**/api/v1/instances/instance-1/recovery-points', async (route) => route.fulfill({ json: [] }))
  await page.route('**/api/v1/instances/instance-1/actions', async (route) => {
    requestBody = route.request().postDataJSON()
    operations.push({
      id: 'repair-1', instance_id: instance.id, actor: 'FLEET_ADMIN', type: 'REPAIR_RUNTIME',
      status: 'SUCCEEDED', summary: 'Restart and verify fleet-test-01', created_at: now, updated_at: now,
    })
    await route.fulfill({ status: 202, json: operations[0] })
  })
  await page.route('**/api/v1/instances/instance-1/observations/refresh', async (route) => route.fulfill({ status: 202, json: {} }))
  page.on('dialog', async (dialog) => dialog.accept())
  await openFleet(page, [instance], operations)

  await expect(page.getByTitle('Repair and verify runtime')).toBeVisible()
  await page.getByRole('button', { name: instance.name }).click()
  await expect(page.getByText('Managed runtime needs repair')).toBeVisible()
  await page.getByRole('button', { name: 'Repair and verify', exact: true }).first().click()
  await expect.poll(() => requestBody).toEqual({ action: 'repair-runtime', confirm_name: instance.name })
})

test('automatic runtime recovery shows bounded progress and can be stopped', async ({ page }) => {
  const instance = {
    id: 'instance-auto-repair',
    name: 'fleet-auto-repair',
    host_id: host.id,
    host_name: host.name,
    status: 'RUNNING',
    image: 'hermes:test',
    image_id: `sha256:${'a'.repeat(64)}`,
    provider: 'openai-codex',
    model: 'gpt-5.6-sol',
    reasoning: 'medium',
    service_tier: 'normal',
    codex_configured: true,
    api_port: 8650,
    dashboard_port: 9130,
    project_name: 'hermes-fleet-auto-repair',
    data_volume: 'hermes-fleet-auto-repair-data',
    managed_path: '/tmp/hermes-fleet-auto-repair',
    created_at: now,
    updated_at: now,
    runtime_remediation: {
      instance_id: 'instance-auto-repair',
      workflow_id: '00000000-0000-4000-8000-000000000031',
      status: 'COOLDOWN',
      phase: 1,
      attempt_in_phase: 3,
      total_attempts: 3,
      max_phases: 3,
      max_attempts: 9,
      consecutive_drift: 4,
      next_attempt_at: '2099-01-01T00:00:00Z',
      last_error: 'Hermes did not become healthy',
      updated_at: now,
    },
    observation: {
      instance_id: 'instance-auto-repair',
      status: 'DEGRADED',
      summary: 'Runtime drift detected',
      received_at: now,
      checks: [{ name: 'runtime', status: 'DRIFT', detail: 'Desired RUNNING state does not match container state or health' }],
    },
  }
  let canceled = false
  await page.route('**/api/v1/instances/instance-auto-repair/recovery-points', async (route) => route.fulfill({ json: [] }))
  await page.route('**/api/v1/instances/instance-auto-repair/runtime-remediation/cancel', async (route) => {
    canceled = true
    await route.fulfill({ json: { ...instance.runtime_remediation, status: 'CANCELED', next_attempt_at: null } })
  })
  page.on('dialog', async (dialog) => dialog.accept())
  await openFleet(page, [instance])

  await expect(page.getByTitle('Restart and verify runtime')).toHaveCount(0)
  await page.getByRole('button', { name: instance.name }).click()
  await expect(page.getByText('Automatic recovery is cooling down')).toBeVisible()
  await expect(page.getByText('Attempt 3 of 9')).toBeVisible()
  await expect(page.getByText('Phase 1 of 3 · restart managed services')).toBeVisible()
  await expect(page.getByText('Hermes did not become healthy')).toBeVisible()
  await page.getByRole('button', { name: 'Stop automatic recovery' }).first().click()
  await expect.poll(() => canceled).toBe(true)
})
