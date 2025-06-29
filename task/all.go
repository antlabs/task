// Copyright 2023-2024 antlabs. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package task imports all available task drivers to ensure they are registered.
package task

import (
	// Import all task drivers to ensure they register themselves
	_ "github.com/antlabs/task/task/io"
	_ "github.com/antlabs/task/task/onebyone"
	_ "github.com/antlabs/task/task/stream"
)
