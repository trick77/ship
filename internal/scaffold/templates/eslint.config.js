// Owned by ship init: the family's ESLint config. ship.yaml's ui.lint runs it with --max-warnings 0.
import js from "@eslint/js";
import tseslint from "typescript-eslint";
import eslintConfigPrettier from "eslint-config-prettier";
[[- if .ReactHooks ]]
import reactHooks from "eslint-plugin-react-hooks";
[[- end ]]

export default tseslint.config(
  js.configs.recommended,
  ...tseslint.configs.recommended,
[[- if .ReactHooks ]]
  reactHooks.configs.flat.recommended,
[[- end ]]
  eslintConfigPrettier,
  {
    rules: {
      "@typescript-eslint/no-unused-vars": [
        "error",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_" },
      ],
      "@typescript-eslint/no-explicit-any": "warn",
    },
  },
  { ignores: ["dist/", "coverage/"] },
);
