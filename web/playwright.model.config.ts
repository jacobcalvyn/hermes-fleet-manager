import { defineConfig } from '@playwright/test'

export default defineConfig({
	testDir: './e2e',
	testMatch: 'hermes-timeline.spec.ts',
	fullyParallel: true,
	reporter: 'line',
})
