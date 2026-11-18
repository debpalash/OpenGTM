import js from '@eslint/js'
import globals from 'globals'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'
import { defineConfig, globalIgnores } from 'eslint/config'

export default defineConfig([
  globalIgnores(['dist']),
  {
    files: ['**/*.{js,jsx}'],
    extends: [
      js.configs.recommended,
      reactHooks.configs.flat.recommended,
      reactRefresh.configs.vite,
    ],
    languageOptions: {
      ecmaVersion: 2020,
      globals: globals.browser,
      parserOptions: {
        ecmaVersion: 'latest',
        ecmaFeatures: { jsx: true },
        sourceType: 'module',
      },
    },
    rules: {
      'no-unused-vars': ['error', { varsIgnorePattern: '^[A-Z_]' }],
      // Native blocking dialogs are banned: use useConfirm()/usePrompt() from
      // @/components/dialog-provider. typescript-eslint does not support TS 7 yet,
      // so .ts/.tsx cannot be parsed by ESLint; tests/no-native-dialogs.test.ts
      // enforces the same rule on src/**/*.{ts,tsx}.
      'no-restricted-globals': ['error',
        ...['confirm', 'prompt', 'alert'].map((name) => ({ name, message: 'Use useConfirm()/usePrompt() instead of native dialogs.' })),
      ],
      'no-restricted-properties': ['error',
        ...['confirm', 'prompt', 'alert'].flatMap((property) =>
          ['window', 'globalThis', 'self'].map((object) => ({ object, property, message: 'Use useConfirm()/usePrompt() instead of native dialogs.' }))),
      ],
    },
  },
])
