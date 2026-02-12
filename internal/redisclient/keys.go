// Package redisclient provides Redis key pattern definitions for the Smart Router application.
package redisclient

import "fmt"

// RedisPrefix is the prefix for all Redis keys in the Smart Router application
const RedisPrefix = "voice:"

// PoolAvailableKey returns the Redis key for available pods in a tier
func PoolAvailableKey(tier string) string {
	return fmt.Sprintf("%spool:%s:available", RedisPrefix, tier)
}

// PoolAssignedKey returns the Redis key for assigned pods in a tier
func PoolAssignedKey(tier string) string {
	return fmt.Sprintf("%spool:%s:assigned", RedisPrefix, tier)
}

// MerchantAssignedKey returns the Redis key for pods assigned to a merchant pool
func MerchantAssignedKey(merchant string) string {
	return fmt.Sprintf("%smerchant:%s:assigned", RedisPrefix, merchant)
}

// MerchantPodsKey returns the Redis key for a merchant's available pods
func MerchantPodsKey(merchant string) string {
	return fmt.Sprintf("%smerchant:%s:pods", RedisPrefix, merchant)
}

// CallInfoKey returns the Redis key for call information
func CallInfoKey(callSID string) string {
	return fmt.Sprintf("%scall:%s", RedisPrefix, callSID)
}

// PodInfoKey returns the Redis key for pod information
func PodInfoKey(podName string) string {
	return fmt.Sprintf("%spod:%s", RedisPrefix, podName)
}

// PodTierKey returns the Redis key for pod tier mapping
func PodTierKey(podName string) string {
	return fmt.Sprintf("%spod:tier:%s", RedisPrefix, podName)
}

// LeaseKey returns the Redis key for pod lease
func LeaseKey(podName string) string {
	return fmt.Sprintf("%slease:%s", RedisPrefix, podName)
}

// DrainingKey returns the Redis key for draining pod status
func DrainingKey(podName string) string {
	return fmt.Sprintf("%spod:draining:%s", RedisPrefix, podName)
}

// PodMetadataKey returns the Redis key for pod metadata hash
func PodMetadataKey() string {
	return RedisPrefix + "pod:metadata"
}

// PodCallsKey returns the Redis key for the SET of active call SIDs on a pod.
// Used to track all concurrent calls on shared pods (and single calls on exclusive pods).
func PodCallsKey(podName string) string {
	return fmt.Sprintf("%spod:%s:calls", RedisPrefix, podName)
}

// TierConfigKey returns the Redis key for the canonical tier configuration.
// See also config.TierConfigRedisKey (the authoritative constant).
func TierConfigKey() string {
	return RedisPrefix + "tier:config"
}
