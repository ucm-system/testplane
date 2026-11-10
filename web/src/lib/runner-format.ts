/** input_format（附录 C，D-67）→ 中文说明（表徽标与提交页提示共用）。 */
export function inputFormatLabel(format: 'jsonl' | 'yaml' | 'text'): string {
  switch (format) {
    case 'jsonl':
      return 'jsonl（逐行 JSON，平台校验结构）'
    case 'yaml':
      return 'yaml（只校语法，语义归 Runner）'
    case 'text':
      return 'text（只校语法，语义归 Runner）'
  }
}

/** 提交页输入框上方的一句话提示（按 input_format）。 */
export function inputHint(format: 'jsonl' | 'yaml' | 'text'): string {
  switch (format) {
    case 'jsonl':
      return 'JSONL：每行一个 JSON 对象（平台校验字段类型与 required，报错带行号）'
    case 'yaml':
      return 'YAML 原文：平台只做语法校验，语义完全归 Runner'
    case 'text':
      return '文本原文：平台只做语法校验，语义完全归 Runner'
  }
}
