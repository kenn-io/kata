import eslint from '@eslint/js'
import svelte from 'eslint-plugin-svelte'
import tseslint from 'typescript-eslint'
import globals from 'globals'

export default tseslint.config(
  eslint.configs.recommended,
  ...tseslint.configs.recommended,
  ...svelte.configs.recommended,
  {
    files: ['src/lib/api/generated/**/*.ts'],
    rules: {
      // Orval emits `any` in its distributive union helpers for read-only schemas.
      '@typescript-eslint/no-explicit-any': 'off',
    },
  },
  {
    languageOptions: {
      globals: globals.browser,
    },
  },
  {
    files: ['**/*.svelte', '**/*.svelte.ts'],
    languageOptions: {
      parserOptions: {
        parser: tseslint.parser,
      },
    },
  },
  {
    files: ['test/**/*.js'],
    languageOptions: {
      globals: {
        Bun: 'readonly',
        URL: 'readonly',
      },
    },
  },
  {
    ignores: ['dist/', 'node_modules/', 'playwright-report/', 'test-results/'],
  },
)
