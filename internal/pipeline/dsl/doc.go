// Package dsl 目前是空的占位包，没有任何实现，也没有被任何包引用。
//
// 管道 DSL 的实际结构见 internal/meta/model.PipelineSpec，
// 解析与校验见 internal/pipeline/compile。YAML 只是提交格式，
// 落库后以 pipeline_version.spec 的快照为准。
//
// 保留还是删除待定：若确定不再需要独立的 dsl 层，直接删掉本包。
package dsl
