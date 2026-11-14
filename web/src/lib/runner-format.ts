/** input_format（附录 C，D-67）→ 中文说明（表徽标与提交页提示共用）。 */
export function inputFormatLabel(format: 'jsonl' | 'yaml' | 'text'): string {
  switch (format) {
    case 'jsonl':
      return 'jsonl'
    case 'yaml':
      return 'yaml'
    case 'text':
      return 'text'
  }
}

/** 提交页输入框上方的一句话提示（按 input_format）。 */
export function inputHint(format: 'jsonl' | 'yaml' | 'text'): string {
  switch (format) {
    case 'jsonl':
      return '每行一个 JSON 对象'
    case 'yaml':
      return 'YAML 原文，只校语法'
    case 'text':
      return '文本原文，只校语法'
  }
}
