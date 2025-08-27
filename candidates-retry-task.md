# Context
Filename: candidates-retry-task.md
Created on: 2025-08-27T12:22:15.486Z
Created by: Collin
Yolo mode: 否

# Task Description
添加candidates为空/异常判断-重试功能。任务分为两步：
1. 定位到response为空-重试的代码
2. 添加candidates为空/异常判断-重试

检查代码逻辑参考：
```python
if response is None:
    raise InvalidApiResponseStructureError(
        response, expected_type_name="types.GenerateContentResponse")

candidates = response.candidates

if not isinstance(candidates, list):
    raise InvalidApiResponseStructureError(
        candidates, expected_type_name="list[types.Candidate]")
```

# Project Overview
Go语言项目，实现Gemini API代理服务，包含流式响应处理和重试机制

⚠️ Warning: Do Not Modify This Section ⚠️
遵循RIPER-5协议：
- RESEARCH: 分析代码结构，理解现有重试机制
- INNOVATE: 探索candidates检查与重试的实现方案  
- PLAN: 制定详细的实现计划
- EXECUTE: 严格按计划实现
- REVIEW: 验证实现是否与计划一致
⚠️ Warning: Do Not Modify This Section ⚠️

# Analysis
## 现有代码结构完整分析

### 核心重试逻辑位置
主要的重试逻辑位于 `streaming/retry.go` 文件中的 `ProcessStreamAndRetryInternally` 函数（第100-438行）。

### SSE数据解析机制
在 `streaming/sse.go` 中实现了完整的SSE数据解析：

#### 关键函数分析
1. **ParseLineContent** (第90-150行): 
   - 解析SSE数据行提取文本内容
   - **已包含candidates检查**: 第106-109行检查 `data["candidates"]` 是否为有效列表
   - **已包含candidate检查**: 第111-114行检查第一个candidate是否有效
   - 但这些检查仅返回空LineContent，**没有触发重试**

2. **ExtractFinishReason** (第54-81行):
   - **已包含candidates检查**: 第71-78行检查candidates列表
   - 同样仅返回空字符串，**没有重试机制**

3. **RemoveDoneTokenFromLine** (第153-230行):
   - **已包含完整的candidates结构检查**: 第169-176行
   - 同样没有重试，仅返回原始line

### 当前重试触发条件
位于 `streaming/retry.go` ProcessStreamAndRetryInternally函数中：
1. HTTP响应错误 (第407-432行)
2. 流式响应中断的各种情况 (第185-213行)
3. 空响应检查 (第198-201行)

### 关键发现
**重要**: 现有代码已经在SSE解析层面实现了candidates结构检查，但这些检查**没有连接到重试机制**。当检测到无效的candidates结构时：
- ParseLineContent返回空的LineContent
- ExtractFinishReason返回空字符串  
- 这导致textChunk为空，但**不会触发重试**

### 缺失的连接点
需要在 `streaming/retry.go` 的ProcessStreamAndRetryInternally函数中添加：
1. 检测到无效response结构时的重试触发
2. candidates为空/无效时的专门重试逻辑
3. 与现有SSE解析函数的错误状态传递机制

# Proposed Solution
## 选定实现方案：扩展现有错误检测机制

### 核心思路
在`streaming/retry.go`的ProcessStreamAndRetryInternally函数中，增加对SSE数据结构有效性的检查。当检测到candidates字段缺失或无效时，触发重试。

### 具体实现策略
1. **新增重试条件检测**:
   - 在现有的重试决策逻辑中添加结构性错误检测
   - 新增interruptionReason类型："INVALID_CANDIDATES"、"MISSING_RESPONSE_DATA"

2. **检测逻辑**:
   - 当IsDataLine(line)返回true但ParseLineContent返回空LineContent时
   - 直接解析JSON检查candidates字段的存在性和有效性
   - 区分"正常空内容"和"结构错误导致的空内容"

3. **集成现有重试框架**:
   - 复用现有的consecutiveRetryCount计数机制
   - 复用现有的重试延迟和错误报告逻辑
   - 保持与现有错误处理的一致性

### 优势
- 最小化代码改动，降低引入bug风险
- 复用成熟的重试框架
- 保持现有函数接口稳定
- 错误处理逻辑集中在一处

# Current Execution Step: "5. 实现完成"

# Task Progress
[2025-08-27T12:22:15.486Z]
- 完成完整代码结构分析
- 识别现有SSE解析中的candidates检查逻辑
- 发现关键问题：检查逻辑存在但未连接重试机制
- 定位需要修改的核心位置：streaming/retry.go ProcessStreamAndRetryInternally函数

[2025-08-27T12:32:40.935Z]
- 成功添加isValidResponseStructure辅助函数到streaming/retry.go
- 在ProcessStreamAndRetryInternally函数中集成candidates检查逻辑
- 修改重试决策逻辑，添加INVALID_CANDIDATES错误类型
- 更新错误日志，包含candidates验证失败的详细信息
- 代码编译测试通过，无语法错误

## 详细实现计划

### 修改文件: `streaming/retry.go`
**目标函数**: ProcessStreamAndRetryInternally (第100-438行)

#### 具体修改位置和内容

1. **第149-153行** - 在ParseLineContent调用后添加candidates检查:
```go
if IsDataLine(line) {
    content := ParseLineContent(line)
    textChunk = content.Text
    isThought = content.IsThought
    
    // NEW: 添加candidates结构检查
    if textChunk == "" && !isThought && isDataLine(line) {
        // 检查是否为结构性错误导致的空内容
        needsValidation := true
    }
}
```

2. **第181-214行** - 在现有重试决策逻辑中添加结构检查:
```go
// 在现有的finishReason := ExtractFinishReason(line)后添加
if needsValidation && IsDataLine(line) {
    if !isValidResponseStructure(line) {
        logger.LogError("Invalid response structure detected - missing or invalid candidates")
        interruptionReason = "INVALID_CANDIDATES"
        needsRetry = true
    }
}
```

3. **新增函数** - 添加响应结构验证函数:
```go
// isValidResponseStructure 检查SSE数据行的响应结构有效性
func isValidResponseStructure(line string) bool {
    // 实现candidates字段检查逻辑
}
```

#### 实现规格
- **函数位置**: 在ProcessStreamAndRetryInternally函数之前添加辅助函数
- **检查逻辑**: 解析JSON，检查candidates字段存在且为有效数组
- **错误类型**: 新增interruptionReason类型"INVALID_CANDIDATES"
- **集成方式**: 插入到现有的重试决策逻辑流程中

#### 错误处理和日志
- 使用现有的logger.LogError记录结构性错误
- 在重试超限时的错误响应中包含结构错误信息
- 保持与现有错误处理格式的一致性

### 实现检查清单
1. 在streaming/retry.go中添加isValidResponseStructure辅助函数
2. 在ProcessStreamAndRetryInternally函数中添加candidates检查逻辑
3. 修改重试决策逻辑，增加INVALID_CANDIDATES条件
4. 更新错误日志，包含新的错误类型
5. 测试验证：模拟无效candidates的响应场景

# Final Review
待完成...