package main

import (
	"fmt"
	"math/rand"
	"time"
)

// 活跃度等级枚举 - 使用 uint8 节省内存
type ActivityLevel uint8

const (
	ActivityLow ActivityLevel = iota
	ActivityMedium
	ActivityHigh
)

// 信息结构体 - 优化数据类型对齐
type Entity struct {
	ID               string              // ID
	LastMatchedUsers map[string]int64    // 用户ID: 时间戳
	Blacklist        map[string]struct{} // 黑名单，使用struct{}节省内存
	MicCount         uint16              // 上麦人数
	AudienceCount    uint16              // 观众人数
	WaitSeconds      uint16              // 等待时间（秒）
	MatchHistory     uint16              // 历史成功匹配次数
	ActivityLevel    ActivityLevel       // 活跃度等级
	_                [1]byte             // padding对齐
}

// 匹配候选结果 - 使用指针减少拷贝
type MatchResult struct {
	Room  *Entity // 匹配的实体
	Score int16   // 匹配分数
	_     [6]byte // padding对齐
}

// 匹配详情 - 用于输出匹配原因
type MatchDetail struct {
	Entity           *Entity
	Score            int16
	WaitScore        int16
	SegmentScore     int16
	AudienceScore    int16
	HistoryScore     int16
	ActivityScore    int16
	CurrentSegment   uint8
	CandidateSegment uint8
	Rejected         bool
	RejectReason     string
}

// 匹配配置 - 将魔数提取为配置
type MatchConfig struct {
	RecentMatchCooldown int64 // 冷却时间（秒）
	MaxWaitTime         int   // 最大等待时间
	MinWaitTime         int   // 最小等待时间
}

var DefaultMatchConfig = MatchConfig{
	RecentMatchCooldown: 600, // 10分钟
	MaxWaitTime:         300, // 5分钟
	MinWaitTime:         20,  // 20秒
}

// 预计算的分段映射 - 避免重复计算
var micSegmentMap = [...]uint8{0, 1, 1, 1, 2, 2, 2, 3, 3, 3, 3, 3, 3, 3, 3, 3}

// 分段判定 - 优化为查表
func getMicSegment(micCount uint16) uint8 {
	if micCount == 0 {
		return 0
	}
	if micCount < uint16(len(micSegmentMap)) {
		return micSegmentMap[micCount]
	}
	return 3
}

// 等待时间得分 - 优化计算
func scoreWaitTime(seconds uint16, config *MatchConfig) int16 {
	if seconds <= uint16(config.MinWaitTime) {
		return 0
	}
	if seconds <= 60 {
		return int16((seconds - uint16(config.MinWaitTime)) / 10)
	}
	return 4 + int16((seconds-60)/10*2)
}

// 上麦人数段一致性得分 - 优化逻辑
func scoreMicSegment(currentSeg, candidateSeg uint8, waitTime uint16) int16 {
	if currentSeg == candidateSeg {
		return 10
	}

	diff := currentSeg - candidateSeg
	if diff < 0 {
		diff = -diff
	}

	if diff == 1 && waitTime >= 60 {
		return 3
	}
	if waitTime >= 60 {
		return 0
	}
	return -999 // 不允许匹配
}

// 观众人数差得分 - 使用查表优化
var audienceDiffScores = [...]int16{5, 4, 3, 2, 1, 0}

func scoreAudienceDiff(diff int) int16 {
	if diff < 0 {
		diff = -diff
	}
	if diff < len(audienceDiffScores) {
		return audienceDiffScores[diff]
	}
	return 0
}

// 历史成功匹配得分 - 优化条件判断
func scoreMatchHistory(history uint16) int16 {
	if history >= 10 {
		return 4
	}
	if history >= 5 {
		return 2
	}
	return 0
}

// 活跃度得分 - 使用数组查表
var activityScores = [...]int16{0, 2, 3} // low, medium, high

func scoreActivity(level ActivityLevel) int16 {
	if level < ActivityLevel(len(activityScores)) {
		return activityScores[level]
	}
	return 0
}

// 快速排除检查 - 提前退出优化
func quickReject(current *Entity, candidate *Entity, currentUserID string, config *MatchConfig, currentTime int64) (bool, string) {
	// 黑名单检查
	if _, exists := candidate.Blacklist[currentUserID]; exists {
		return true, "用户在黑名单中"
	}

	// 冷却时间检查
	if lastTime, ok := candidate.LastMatchedUsers[currentUserID]; ok {
		if currentTime-lastTime < config.RecentMatchCooldown {
			return true, fmt.Sprintf("冷却时间未满（%d秒前匹配过）", currentTime-lastTime)
		}
	}

	// 段位检查 - 如果等待时间不够且段位差距过大则排除
	if candidate.WaitSeconds < 60 {
		currentSeg := getMicSegment(current.MicCount)
		candidateSeg := getMicSegment(candidate.MicCount)
		diff := currentSeg - candidateSeg
		if diff < 0 {
			diff = -diff
		}
		if diff > 1 {
			return true, fmt.Sprintf("等待时间不足且段位差距过大（当前段位%d，候选段位%d）", currentSeg, candidateSeg)
		}
	}

	return false, ""
}

// 主打分逻辑 - 优化计算顺序和缓存，返回详细信息
func scoreMatchDetailed(current *Entity, candidate *Entity, currentUserID string, config *MatchConfig, currentTime int64, currentSeg uint8) *MatchDetail {
	detail := &MatchDetail{
		Entity:           candidate,
		CurrentSegment:   currentSeg,
		CandidateSegment: getMicSegment(candidate.MicCount),
	}

	// 快速排除检查
	rejected, reason := quickReject(current, candidate, currentUserID, config, currentTime)
	if rejected {
		detail.Rejected = true
		detail.RejectReason = reason
		detail.Score = -999
		return detail
	}

	// 计算各项得分
	detail.WaitScore = scoreWaitTime(candidate.WaitSeconds, config)
	detail.SegmentScore = scoreMicSegment(currentSeg, detail.CandidateSegment, candidate.WaitSeconds)

	if detail.SegmentScore < 0 {
		detail.Rejected = true
		detail.RejectReason = "段位不匹配"
		detail.Score = -999
		return detail
	}

	detail.AudienceScore = scoreAudienceDiff(int(current.AudienceCount) - int(candidate.AudienceCount))
	detail.HistoryScore = scoreMatchHistory(candidate.MatchHistory)
	detail.ActivityScore = scoreActivity(candidate.ActivityLevel)

	detail.Score = detail.WaitScore + detail.SegmentScore + detail.AudienceScore + detail.HistoryScore + detail.ActivityScore
	return detail
}

// 主打分逻辑 - 优化计算顺序和缓存
func scoreMatch(current *Entity, candidate *Entity, currentUserID string, config *MatchConfig, currentTime int64, currentSeg uint8) int16 {
	detail := scoreMatchDetailed(current, candidate, currentUserID, config, currentTime, currentSeg)
	return detail.Score
}

// 匹配逻辑 - 优化内存分配和算法，返回详细信息
func matchEntityDetailed(current *Entity, pool []*Entity, currentUserID string, config *MatchConfig) (*Entity, []*MatchDetail) {
	if len(pool) == 0 {
		return nil, nil
	}

	// 预分配结果切片，避免频繁扩容
	details := make([]*MatchDetail, 0, len(pool))
	maxScore := int16(-1000)
	currentTime := time.Now().Unix()
	currentSeg := getMicSegment(current.MicCount)

	// 计算所有候选的详细信息
	for i := range pool {
		detail := scoreMatchDetailed(current, pool[i], currentUserID, config, currentTime, currentSeg)
		details = append(details, detail)

		if !detail.Rejected && detail.Score > maxScore {
			maxScore = detail.Score
		}
	}

	// 如果没有有效匹配
	if maxScore < 0 {
		return nil, details
	}

	// 收集所有最高分的候选
	candidates := make([]*Entity, 0, len(pool))
	for _, detail := range details {
		if !detail.Rejected && detail.Score == maxScore {
			candidates = append(candidates, detail.Entity)
		}
	}

	// 随机选择一个最高分候选
	if len(candidates) == 0 {
		return nil, details
	}

	selected := candidates[rand.Intn(len(candidates))]
	return selected, details
}

// 匹配逻辑 - 优化内存分配和算法
func matchEntity(current *Entity, pool []*Entity, currentUserID string, config *MatchConfig) *Entity {
	if len(pool) == 0 {
		return nil
	}

	// 预分配结果切片，避免频繁扩容
	results := make([]*MatchResult, 0, len(pool))
	maxScore := int16(-1000)
	currentTime := time.Now().Unix()
	currentSeg := getMicSegment(current.MicCount)

	// 第一遍：找到最高分数
	for i := range pool {
		score := scoreMatch(current, pool[i], currentUserID, config, currentTime, currentSeg)
		if score > maxScore {
			maxScore = score
		}
	}

	// 如果没有有效匹配
	if maxScore < 0 {
		return nil
	}

	// 第二遍：收集所有最高分的候选
	for i := range pool {
		score := scoreMatch(current, pool[i], currentUserID, config, currentTime, currentSeg)
		if score == maxScore {
			results = append(results, &MatchResult{
				Room:  pool[i],
				Score: score,
			})
		}
	}

	// 随机选择一个最高分候选
	if len(results) == 0 {
		return nil
	}

	// 使用更好的随机数生成
	selected := results[rand.Intn(len(results))]
	return selected.Room
}

// 批量匹配优化 - 为多个同时匹配
func batchMatchEntities(rooms []*Entity, pool []*Entity, userIDs []string, config *MatchConfig) map[string]*Entity {
	if len(rooms) != len(userIDs) {
		panic("rooms and userIDs length mismatch")
	}

	results := make(map[string]*Entity, len(rooms))

	// 为每个进行匹配
	for i, room := range rooms {
		if room == nil || i >= len(userIDs) {
			continue
		}

		userID := userIDs[i]
		matched := matchEntity(room, pool, userID, config)
		if matched != nil {
			results[room.ID] = matched
		}
	}

	return results
}

// 随机生成实体
func generateRandomEntity(id string) *Entity {
	// 生成随机的历史匹配用户（可能为空）
	lastMatchedUsers := make(map[string]int64)
	if rand.Float32() < 0.3 { // 30%概率有历史匹配
		numUsers := rand.Intn(3) + 1 // 1-3个用户
		for i := 0; i < numUsers; i++ {
			userID := fmt.Sprintf("user%d", rand.Intn(1000))
			// 随机时间，0-1200秒前（0-20分钟）
			lastMatchedUsers[userID] = time.Now().Unix() - int64(rand.Intn(1201))
		}
	}

	// 生成随机黑名单（可能为空）
	blacklist := make(map[string]struct{})
	if rand.Float32() < 0.2 { // 20%概率有黑名单
		numBlacklisted := rand.Intn(2) + 1 // 1-2个用户
		for i := 0; i < numBlacklisted; i++ {
			userID := fmt.Sprintf("user%d", rand.Intn(1000))
			blacklist[userID] = struct{}{}
		}
	}

	return &Entity{
		ID:               id,
		MicCount:         uint16(rand.Intn(15) + 1),   // 1-15人
		AudienceCount:    uint16(rand.Intn(200) + 10), // 10-209人
		WaitSeconds:      uint16(rand.Intn(300) + 10), // 10-309秒
		MatchHistory:     uint16(rand.Intn(20)),       // 0-19次
		ActivityLevel:    ActivityLevel(rand.Intn(3)), // 0-2 (Low, Medium, High)
		LastMatchedUsers: lastMatchedUsers,
		Blacklist:        blacklist,
	}
}

// 生成随机实体池
func generateEntityPool(count int) []*Entity {
	entities := make([]*Entity, count)
	for i := 0; i < count; i++ {
		entities[i] = generateRandomEntity(fmt.Sprintf("entity_%03d", i+1))
	}
	return entities
}

// 输出匹配详情
func printMatchDetails(current *Entity, matched *Entity, details []*MatchDetail) {
	fmt.Printf("\n=== 匹配详情 ===\n")
	fmt.Printf("当前实体: %s (麦位:%d, 观众:%d, 等待:%d秒, 段位:%d)\n",
		current.ID, current.MicCount, current.AudienceCount, current.WaitSeconds, getMicSegment(current.MicCount))

	if matched != nil {
		fmt.Printf("✅ 匹配成功: %s\n", matched.ID)

		// 找到匹配的实体详情
		for _, detail := range details {
			if detail.Entity.ID == matched.ID {
				fmt.Printf("匹配原因:\n")
				fmt.Printf("  - 等待时间得分: %d (等待%d秒)\n", detail.WaitScore, detail.Entity.WaitSeconds)
				fmt.Printf("  - 段位得分: %d (当前段位%d, 候选段位%d)\n", detail.SegmentScore, detail.CurrentSegment, detail.CandidateSegment)
				fmt.Printf("  - 观众差异得分: %d (观众差%d)\n", detail.AudienceScore, int(current.AudienceCount)-int(detail.Entity.AudienceCount))
				fmt.Printf("  - 历史得分: %d (历史匹配%d次)\n", detail.HistoryScore, detail.Entity.MatchHistory)
				fmt.Printf("  - 活跃度得分: %d (%s)\n", detail.ActivityScore, detail.Entity.ActivityLevel.String())
				fmt.Printf("  - 总分: %d\n", detail.Score)
				break
			}
		}

		// 显示前5名候选
		fmt.Printf("\n前5名候选:\n")
		validCandidates := make([]*MatchDetail, 0)
		for _, detail := range details {
			if !detail.Rejected {
				validCandidates = append(validCandidates, detail)
			}
		}

		// 简单排序（冒泡排序，因为数量不大）
		for i := 0; i < len(validCandidates)-1; i++ {
			for j := 0; j < len(validCandidates)-1-i; j++ {
				if validCandidates[j].Score < validCandidates[j+1].Score {
					validCandidates[j], validCandidates[j+1] = validCandidates[j+1], validCandidates[j]
				}
			}
		}

		displayCount := len(validCandidates)
		if displayCount > 5 {
			displayCount = 5
		}

		for i := 0; i < displayCount; i++ {
			detail := validCandidates[i]
			selected := ""
			if detail.Entity.ID == matched.ID {
				selected = " ⭐"
			}
			fmt.Printf("  %d. %s (分数:%d, 麦位:%d, 观众:%d, 等待:%ds)%s\n",
				i+1, detail.Entity.ID, detail.Score, detail.Entity.MicCount,
				detail.Entity.AudienceCount, detail.Entity.WaitSeconds, selected)
		}

	} else {
		fmt.Printf("❌ 未找到匹配\n")

		// 显示被拒绝的原因统计
		rejectReasons := make(map[string]int)
		for _, detail := range details {
			if detail.Rejected {
				rejectReasons[detail.RejectReason]++
			}
		}

		fmt.Printf("拒绝原因统计:\n")
		for reason, count := range rejectReasons {
			fmt.Printf("  - %s: %d个\n", reason, count)
		}
	}
}

// 辅助函数：创建活跃度枚举
func ParseActivityLevel(level string) ActivityLevel {
	switch level {
	case "high":
		return ActivityHigh
	case "medium":
		return ActivityMedium
	default:
		return ActivityLow
	}
}

// 辅助函数：转换活跃度为字符串
func (a ActivityLevel) String() string {
	switch a {
	case ActivityHigh:
		return "high"
	case ActivityMedium:
		return "medium"
	default:
		return "low"
	}
}

// 示例用法
func main() {
	// 初始化随机种子
	rand.Seed(time.Now().UnixNano())

	// 随机生成100个候选实体
	fmt.Println("正在生成100个随机实体...")
	candidates := generateEntityPool(100)
	fmt.Printf("生成完成！候选实体数量: %d\n", len(candidates))

	// 创建当前实体
	current := &Entity{
		ID:               "current",
		MicCount:         3,
		AudienceCount:    50,
		WaitSeconds:      80,
		Blacklist:        make(map[string]struct{}),
		LastMatchedUsers: make(map[string]int64),
	}

	// 进行详细匹配
	fmt.Printf("\n开始匹配实体 %s...\n", current.ID)
	matched, details := matchEntityDetailed(current, candidates, "user123", &DefaultMatchConfig)

	// 输出详细的匹配信息
	printMatchDetails(current, matched, details)

	// 统计信息
	fmt.Printf("\n=== 统计信息 ===\n")
	totalCandidates := len(candidates)
	rejectedCount := 0
	validCount := 0

	for _, detail := range details {
		if detail.Rejected {
			rejectedCount++
		} else {
			validCount++
		}
	}

	fmt.Printf("总候选数: %d\n", totalCandidates)
	fmt.Printf("有效候选: %d (%.1f%%)\n", validCount, float64(validCount)/float64(totalCandidates)*100)
	fmt.Printf("被拒绝: %d (%.1f%%)\n", rejectedCount, float64(rejectedCount)/float64(totalCandidates)*100)
}
