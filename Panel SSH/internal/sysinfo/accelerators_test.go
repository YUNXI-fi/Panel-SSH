package sysinfo

import "testing"

const sampleNvidiaCSV = `0, NVIDIA GeForce GTX 1650, 0, 5, 4096, 38
1, NVIDIA GeForce RTX 4090, [N/A], 12000, 24564, 52
`

func TestParseNVIDIA(t *testing.T) {
	gpus := parseNVIDIA(sampleNvidiaCSV)
	if len(gpus) != 2 {
		t.Fatalf("GPU 数量错误：%+v", gpus)
	}

	g0 := gpus[0]
	if g0.Kind != KindGPU || g0.ID != 0 || g0.Name != "NVIDIA GeForce GTX 1650" {
		t.Fatalf("GPU0 基本字段错误：%+v", g0)
	}
	if g0.UtilPercent != 0 || g0.TempC != 38 {
		t.Fatalf("GPU0 利用率/温度错误：%+v", g0)
	}
	// MiB 必须换算成字节。
	if g0.MemUsed != 5*1024*1024 || g0.MemTotal != 4096*1024*1024 {
		t.Fatalf("GPU0 显存换算错误：%+v", g0)
	}

	// 第二张卡利用率 [N/A]：必须记为 -1（未知），不能当成 0% 空闲。
	g1 := gpus[1]
	if g1.UtilPercent != -1 {
		t.Fatalf("[N/A] 利用率应解析为 -1，得到 %v", g1.UtilPercent)
	}
	if g1.MemTotal != 24564*1024*1024 {
		t.Fatalf("GPU1 显存总量错误：%d", g1.MemTotal)
	}
}

func TestParseNVIDIASkipsGarbage(t *testing.T) {
	// 告警/表头行混进 stdout 时必须被跳过，不能产生假数据。
	const raw = `WARNING: Ignoring corrupted PerfCap
index, name, utilization.gpu
0, NVIDIA GeForce GTX 1650, 12, 100, 4096, 40
`
	gpus := parseNVIDIA(raw)
	if len(gpus) != 1 || gpus[0].UtilPercent != 12 {
		t.Fatalf("非数据行未被正确跳过：%+v", gpus)
	}
}

// sampleNPU310B1 取自昇腾官方文档的 npu-smi info 原样输出（精简了边框宽度）。
const sampleNPU310B1 = `+-------------------------------+-----------------+------------------------------------------------------+
| NPU Name                      | Health          | Power(W) Temp(C) Hugepages-Usage(page)               |
| Chip Device                   | Bus-Id          | AICore(%) Memory-Usage(MB)                          |
+===============================+=================+======================================================+
| 0 310B1                       | OK              | 10.4 66 15 / 15                                      |
| 0 0                           | NA              | 0 957 / 10810                                        |
+===============================+=================+======================================================+
`

// sampleNPU910B 是 8 卡 910B 输出的前两张，验证多卡时型号/温度的配对关系。
const sampleNPU910B = `+------------------------------------------------------------------------------------------------+
| npu-smi 24.1.rc3                 Version: 24.1.rc3                                             |
+---------------------------+---------------+----------------------------------------------------+
| NPU   Name                | Health        | Power(W)    Temp(C)           Hugepages-Usage(page)|
| Chip                      | Bus-Id        | AICore(%)   Memory-Usage(MB)                      |
+===========================+===============+====================================================+
| 0     910B                | OK            | 300.5       45                0 / 0                |
| 0                         | 0000:C1:00.0  | 100         32768 / 65536                         |
+===========================+===============+====================================================+
| 1     910B                | OK            | 298.1       47                0 / 0                |
| 0                         | 0000:C2:00.0  | 82          30000 / 65536                         |
+===========================+===============+====================================================+
`

func TestParseNPU310B1(t *testing.T) {
	npus := parseNPU(sampleNPU310B1)
	if len(npus) != 1 {
		t.Fatalf("NPU 数量错误：%+v", npus)
	}
	n := npus[0]
	if n.Kind != KindNPU || n.ID != 0 || n.Chip != 0 || n.Name != "310B1" {
		t.Fatalf("NPU 基本字段错误：%+v", n)
	}
	if n.UtilPercent != 0 || n.TempC != 66 {
		t.Fatalf("NPU 利用率/温度错误：%+v", n)
	}
	if n.MemUsed != 957*1024*1024 || n.MemTotal != 10810*1024*1024 {
		t.Fatalf("NPU 显存换算错误：%+v", n)
	}
}

func TestParseNPU910BMultiCard(t *testing.T) {
	npus := parseNPU(sampleNPU910B)
	if len(npus) != 2 {
		t.Fatalf("应解析出两块 NPU，得到 %+v", npus)
	}
	// 第二块卡的温度/利用率必须配到第二张卡上，不能串行复用第一行。
	n1 := npus[1]
	if n1.ID != 1 || n1.Name != "910B" || n1.TempC != 47 || n1.UtilPercent != 82 {
		t.Fatalf("NPU1 配对错误：%+v", n1)
	}
	if n1.MemUsed != 30000*1024*1024 || n1.MemTotal != 65536*1024*1024 {
		t.Fatalf("NPU1 显存错误：%+v", n1)
	}
}

// sampleNPU310B3DualCore 验证一张 NPU 下多个芯片行的场景。
const sampleNPU310B3DualCore = `| 0 310B4 | OK | 8.0 55 0 / 0 |
| 0 0     | NA | 10 500 / 10810 |
| 0 1     | NA | 20 600 / 10810 |
`

func TestParseNPUDualCore(t *testing.T) {
	npus := parseNPU(sampleNPU310B3DualCore)
	if len(npus) != 2 {
		t.Fatalf("双核应解析为两条芯片记录，得到 %+v", npus)
	}
	if npus[0].Chip != 0 || npus[1].Chip != 1 {
		t.Fatalf("芯片编号错误：%+v", npus)
	}
	// 型号与温度来自同一条 NPU 行，两个芯片共享。
	for _, n := range npus {
		if n.Name != "310B4" || n.TempC != 55 {
			t.Fatalf("芯片未正确继承 NPU 型号/温度：%+v", n)
		}
	}
	if npus[1].UtilPercent != 20 || npus[1].MemUsed != 600*1024*1024 {
		t.Fatalf("第二核读数错误：%+v", npus[1])
	}
}

func TestParseAcceleratorsEmpty(t *testing.T) {
	// 没有装 nvidia-smi / npu-smi 的机器：两节为空，返回非 nil 空切片（序列化为 []）。
	out := parseAccelerators("", "")
	if out == nil || len(out) != 0 {
		t.Fatalf("空输入应返回非 nil 空切片，得到 %#v", out)
	}
}
