// Storybook Vitest setup — apply project annotations so every story-as-test
// renders with the same decorators, parameters, and globals as the real
// Storybook UI (including a11y checks).
//
// Since @storybook/addon-vitest 10.3+ the addon applies preview + addon
// annotations automatically via the storybookTest plugin. This file is kept
// for any future custom beforeAll logic (e.g. MSW setup, fake timers).
// See: writing-storybook §6.2.
