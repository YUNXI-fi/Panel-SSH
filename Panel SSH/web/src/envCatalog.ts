/**
 * 软件环境目录的分类标识与显示名。
 *
 * 后端只给标识（runtime / toolchain / …），显示名是界面决定；
 * 应用中心的「系统组件」页签与软件环境窗口共用这一份，避免两处各写一遍后失配。
 * 遇到未知分类时回落到标识本身。
 */
export const ENV_CATEGORY_TEXT: Record<string, string> = {
  runtime: '运行环境',
  toolchain: '开发工具链',
  service: '服务与中间件',
  database: '数据库与缓存',
  monitor: '监控与磁盘',
  utility: '终端效率',
  network: '网络诊断',
  security: '网络安全',
  backup: '备份与同步',
};

/**
 * 分类的展示顺序。
 *
 * 后端加分类时这里不用改，但"哪一类排在前面"是界面主张：语言运行时最高频，
 * 服务与数据库是装完即常驻的重物，日常小工具排在它们之后。
 * 没列在这里的分类按数据顺序排在最后。
 */
export const ENV_CATEGORY_ORDER = [
  'runtime',
  'toolchain',
  'service',
  'database',
  'monitor',
  'utility',
  'network',
  'security',
  'backup',
];
