# Chinese→English Glossary for workbuddy-gateway

Translate every Chinese string (comments, log messages, user-facing text, test
assertions, doc text) to natural English. Keep identifiers, code structure,
numbers, and test logic intact. Update test assertions to match translated
source strings exactly.

## Common term mappings (use these consistently)

Results/Status: 结果→Result, 成功→Success/Successful, 失败→Failed/Failure, 通过→Passed, 拒绝→Rejected, 状态→Status, 状态码→Status code, 返回状态码→Response status code, 未知→Unknown, 空→Empty, 异常→Exception, 可用→Available, 完成→Completed, 通过→Passed
Process: 阶段→Stage, 原因→Reason, 说明→Note, 业务影响→Business impact, 耗时→Duration, 发生阶段→Stage, 将→will, 为→for
Entities: 账号→Account, 模型→Model, 站点→Site, 凭据→Credential, 凭据文件→Credential file, 配置→Config/Configuration, 字段→Field, 文件→File, 端口→Port, 目标→Target, 类型→Type, 来源→Source, 请求→Request, 管理→Admin, 更新→Update, 返回→Returned, 缺少→Missing, 验证→Verify, 判定→Verdict, 默认→Default
Sites: 国内站→Domestic site, 国际站→International site, 内部站→Internal site
Money: 免费→Free, 收费→Paid, 付费耗尽→Paid exhausted, 倍率未知→Unknown multiplier, 官方倍率→Official multiplier, 促销→Promo, 促销已过期→Promo expired, 试用→Trial
Time: 每日签到→Daily checkin, 实时接口→Live API
Account status: 账号状态→Account status, 账号新增→Account added
Misc: 仅支持→Only supports, 参数校验→Parameter validation, 配置编辑→Config edit, 单元测试→Unit test, 个→(counter), 条→(counter), 次→(counter), 项→item, 与→and, 或→or, 和→and, 的→of, 从→from, 是→is, 仅→only, 不→not, 无→none, 已→already, 请→please, 时→when, 后→after, 前→before, 中→in, 上→up, 下→down, 内→inside, 外→outside, 间→between, 分→minute, 秒→second, 天→day, 年→year, 月→month, 日→day, 时→hour, 条→(item), 总→Total, 首→First, 将→will, 为→for

Connectors: 、→, ；→; 。→. ，→, （→( ）→) ：→: ？→? —→—

Keep code identifiers, struct fields, JSON keys, and test function names unchanged.
Keep numbers, URLs, paths unchanged.
Translate log tag prefixes like [管理请求拦截] → [Admin request interception].
