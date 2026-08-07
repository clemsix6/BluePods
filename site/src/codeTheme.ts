import type { ThemeRegistration } from 'shiki';

/// Syntax colours drawn from the page palette, so a code block reads as part of
/// the sheet rather than as a terminal pasted onto it.
export const codeTheme: ThemeRegistration = {
  name: 'linen',
  type: 'light',
  colors: {
    'editor.background': 'transparent',
    'editor.foreground': '#2b0f0e',
  },
  settings: [
    {
      scope: ['comment', 'punctuation.definition.comment'],
      settings: { foreground: '#8b998a', fontStyle: 'italic' },
    },
    {
      scope: ['keyword', 'storage', 'storage.type', 'keyword.control', 'keyword.operator'],
      settings: { foreground: '#6f1d1b' },
    },
    {
      scope: ['entity.name.function', 'support.function', 'meta.function-call'],
      settings: { foreground: '#2b0f0e', fontStyle: 'bold' },
    },
    {
      scope: ['entity.name.type', 'support.type', 'entity.name.namespace'],
      settings: { foreground: '#8a4034' },
    },
    {
      scope: ['string', 'string.quoted', 'constant.character'],
      settings: { foreground: '#5c6e5a' },
    },
    {
      scope: ['constant.numeric', 'constant.language', 'variable.other.constant'],
      settings: { foreground: '#5c6e5a' },
    },
    {
      scope: ['punctuation', 'meta.brace'],
      settings: { foreground: 'rgba(43, 15, 14, 0.5)' },
    },
    {
      scope: ['variable', 'variable.parameter', 'entity.name.variable'],
      settings: { foreground: '#2b0f0e' },
    },
  ],
};
