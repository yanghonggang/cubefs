// Copyright 2024 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package mock

//go:generate mockgen -source=../catalog/catalog.go -destination=./mock_catalog.go -package=mock -mock_names ShardGetter=MockShardGetter
//go:generate mockgen -source=../storage/shard.go -destination=./mock_shard.go -package=mock -mock_names ShardHandler=MockSpaceShardHandler
//go:generate mockgen -source=../base/transport.go -destination=../base/mock_transport.go -package=base -mock_names Transport=MockTransport
//go:generate mockgen -source=../catalog/allocator/allocator.go -destination=./mock_allocator.go -package=mock -mock_names Allocator=MockAllocator
